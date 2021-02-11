package cloudwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana/pkg/plugins/backendplugin"
	"github.com/grafana/grafana/pkg/plugins/backendplugin/coreplugin"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/cloudwatch/cloudwatchiface"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs/cloudwatchlogsiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi/resourcegroupstaggingapiiface"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/setting"
)

type datasourceInfo struct {
	Profile       string
	Region        string
	AuthType      authType
	AssumeRoleARN string
	ExternalID    string
	Namespace     string

	AccessKey string
	SecretKey string
}

const cloudWatchTSFormat = "2006-01-02 15:04:05.000"
const defaultRegion = "default"

// Constants also defined in datasource/cloudwatch/datasource.ts
const logIdentifierInternal = "__log__grafana_internal__"
const logStreamIdentifierInternal = "__logstream__grafana_internal__"

var plog = log.New("tsdb.cloudwatch")
var aliasFormat = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)

func init() {
	registry.Register(&registry.Descriptor{
		Name:         "CloudWatchService",
		InitPriority: registry.Low,
		Instance:     &CloudWatchService{},
	})
}

type CloudWatchService struct {
	LogsService          *LogsService          `inject:""`
	BackendPluginManager backendplugin.Manager `inject:""`
}

func (s *CloudWatchService) Init() error {
	plog.Debug("initing")

	im := datasource.NewInstanceManager(NewInstanceSettings())

	factory := coreplugin.New(backend.ServeOpts{
		QueryDataHandler: newExecutor(s.LogsService, im),
	})

	if err := s.BackendPluginManager.Register("cloudwatch", factory); err != nil {
		plog.Error("Failed to register plugin", "error", err)
	}
	return nil
}

func newExecutor(logsService *LogsService, im instancemgmt.InstanceManager) *cloudWatchExecutor {
	return &cloudWatchExecutor{
		logsService: logsService,
		im:          im,
	}
}

func NewInstanceSettings() datasource.InstanceFactoryFunc {
	return func(settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
		var jsonData map[string]string

		err := json.Unmarshal(settings.JSONData, &jsonData)
		if err != nil {
			return nil, fmt.Errorf("error reading settings: %w", err)
		}

		model := datasourceInfo{
			Profile:       jsonData["profile"],
			Region:        jsonData["defaultRegion"],
			AssumeRoleARN: jsonData["assumeRoleArn"],
			ExternalID:    jsonData["externalId"],
			Namespace:     jsonData["customMetricsNamespaces"],
		}

		atStr := jsonData["authType"]
		at := authTypeDefault
		switch atStr {
		case "credentials":
			at = authTypeSharedCreds
		case "keys":
			at = authTypeKeys
		case "default":
			at = authTypeDefault
		case "arn":
			at = authTypeDefault
			plog.Warn("Authentication type \"arn\" is deprecated, falling back to default")
		default:
			plog.Warn("Unrecognized AWS authentication type", "type", atStr)
		}

		model.AuthType = at

		if model.Profile == "" {
			model.Profile = settings.Database // legacy support
		}

		model.AccessKey = settings.DecryptedSecureJSONData["accessKey"]
		model.SecretKey = settings.DecryptedSecureJSONData["secretKey"]

		return model, nil
	}
}

// cloudWatchExecutor executes CloudWatch requests.
type cloudWatchExecutor struct {
	*models.DataSource //replace with instance manager?

	ec2Client  ec2iface.EC2API
	rgtaClient resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI

	dsInstanceSettings *backend.DataSourceInstanceSettings
	logsService        *LogsService

	im instancemgmt.InstanceManager
}

func (e *cloudWatchExecutor) newSession(region string, pluginCtx backend.PluginContext) (*session.Session, error) {
	dsInfo := e.getDSInfo(pluginCtx)

	bldr := strings.Builder{}
	for i, s := range []string{
		dsInfo.AuthType.String(), dsInfo.AccessKey, dsInfo.Profile, dsInfo.AssumeRoleARN, region,
	} {
		if i != 0 {
			bldr.WriteString(":")
		}
		bldr.WriteString(strings.ReplaceAll(s, ":", `\:`))
	}
	cacheKey := bldr.String()

	sessCacheLock.RLock()
	if env, ok := sessCache[cacheKey]; ok {
		if env.expiration.After(time.Now().UTC()) {
			sessCacheLock.RUnlock()
			return env.session, nil
		}
	}
	sessCacheLock.RUnlock()

	cfgs := []*aws.Config{
		{
			CredentialsChainVerboseErrors: aws.Bool(true),
		},
	}

	var regionCfg *aws.Config
	if dsInfo.Region == defaultRegion {
		plog.Warn("Region is set to \"default\", which is unsupported")
		dsInfo.Region = ""
	}
	if dsInfo.Region != "" {
		regionCfg = &aws.Config{Region: aws.String(dsInfo.Region)}
		cfgs = append(cfgs, regionCfg)
	}

	switch dsInfo.AuthType {
	case authTypeSharedCreds:
		plog.Debug("Authenticating towards AWS with shared credentials", "profile", dsInfo.Profile,
			"region", dsInfo.Region)
		cfgs = append(cfgs, &aws.Config{
			Credentials: credentials.NewSharedCredentials("", dsInfo.Profile),
		})
	case authTypeKeys:
		plog.Debug("Authenticating towards AWS with an access key pair", "region", dsInfo.Region)
		cfgs = append(cfgs, &aws.Config{
			Credentials: credentials.NewStaticCredentials(dsInfo.AccessKey, dsInfo.SecretKey, ""),
		})
	case authTypeDefault:
		plog.Debug("Authenticating towards AWS with default SDK method", "region", dsInfo.Region)
	default:
		panic(fmt.Sprintf("Unrecognized authType: %d", dsInfo.AuthType))
	}
	sess, err := newSession(cfgs...)
	if err != nil {
		return nil, err
	}

	duration := stscreds.DefaultDuration
	expiration := time.Now().UTC().Add(duration)
	if dsInfo.AssumeRoleARN != "" {
		// We should assume a role in AWS
		plog.Debug("Trying to assume role in AWS", "arn", dsInfo.AssumeRoleARN)

		cfgs := []*aws.Config{
			{
				CredentialsChainVerboseErrors: aws.Bool(true),
			},
			{
				Credentials: newSTSCredentials(sess, dsInfo.AssumeRoleARN, func(p *stscreds.AssumeRoleProvider) {
					// Not sure if this is necessary, overlaps with p.Duration and is undocumented
					p.Expiry.SetExpiration(expiration, 0)
					p.Duration = duration
					if dsInfo.ExternalID != "" {
						p.ExternalID = aws.String(dsInfo.ExternalID)
					}
				}),
			},
		}
		if regionCfg != nil {
			cfgs = append(cfgs, regionCfg)
		}
		sess, err = newSession(cfgs...)
		if err != nil {
			return nil, err
		}
	}

	plog.Debug("Successfully created AWS session")

	sessCacheLock.Lock()
	sessCache[cacheKey] = envelope{
		session:    sess,
		expiration: expiration,
	}
	sessCacheLock.Unlock()

	return sess, nil
}

func (e *cloudWatchExecutor) getCWClient(region string, pluginCtx backend.PluginContext) (cloudwatchiface.CloudWatchAPI, error) {
	sess, err := e.newSession(region, pluginCtx)
	if err != nil {
		return nil, err
	}
	return NewCWClient(sess), nil
}

func (e *cloudWatchExecutor) getCWLogsClient(region string, pluginCtx backend.PluginContext) (cloudwatchlogsiface.CloudWatchLogsAPI, error) {
	sess, err := e.newSession(region, pluginCtx)
	if err != nil {
		return nil, err
	}

	logsClient := NewCWLogsClient(sess)

	return logsClient, nil
}

func (e *cloudWatchExecutor) getEC2Client(region string, pluginCtx backend.PluginContext) (ec2iface.EC2API, error) {
	if e.ec2Client != nil {
		return e.ec2Client, nil
	}

	sess, err := e.newSession(region, pluginCtx)
	if err != nil {
		return nil, err
	}
	e.ec2Client = newEC2Client(sess)

	return e.ec2Client, nil
}

func (e *cloudWatchExecutor) getRGTAClient(region string, pluginCtx backend.PluginContext) (resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI,
	error) {
	if e.rgtaClient != nil {
		return e.rgtaClient, nil
	}

	sess, err := e.newSession(region, pluginCtx)
	if err != nil {
		return nil, err
	}
	e.rgtaClient = newRGTAClient(sess)

	return e.rgtaClient, nil
}

func (e *cloudWatchExecutor) alertQuery(ctx context.Context, logsClient cloudwatchlogsiface.CloudWatchLogsAPI,
	queryContext backend.DataQuery, model *simplejson.Json) (*cloudwatchlogs.GetQueryResultsOutput, error) {
	const maxAttempts = 8
	const pollPeriod = 1000 * time.Millisecond

	startQueryOutput, err := e.executeStartQuery(ctx, logsClient, model, queryContext.TimeRange)
	if err != nil {
		return nil, err
	}

	requestParams := simplejson.NewFromAny(map[string]interface{}{
		"region":  model.Get("region").MustString(""),
		"queryId": *startQueryOutput.QueryId,
	})

	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()

	attemptCount := 1
	for range ticker.C {
		res, err := e.executeGetQueryResults(ctx, logsClient, requestParams)
		if err != nil {
			return nil, err
		}
		if isTerminated(*res.Status) {
			return res, err
		}
		if attemptCount >= maxAttempts {
			return res, fmt.Errorf("fetching of query results exceeded max number of attempts")
		}

		attemptCount++
	}

	return nil, nil
}

// Query executes a CloudWatch query.
func (e *cloudWatchExecutor) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	e.dsInstanceSettings = req.PluginContext.DataSourceInstanceSettings

	resp := backend.NewQueryDataResponse()

	var err error
	for _, q := range req.Queries {
		model, err := simplejson.NewJson(q.JSON)
		if err != nil {
			continue
		}

		/*
			Unlike many other data sources,	with Cloudwatch Logs query requests don't receive the results as the response to the query, but rather
			an ID is first returned. Following this, a client is expected to send requests along with the ID until the status of the query is complete,
			receiving (possibly partial) results each time. For queries made via dashboards and Explore, the logic of making these repeated queries is handled on
			the frontend, but because alerts are executed on the backend the logic needs to be reimplemented here.
		*/
		_, fromAlert := req.Headers["FromAlert"]
		isLogAlertQuery := fromAlert && model.Get("queryMode").MustString("") == "Logs"

		if isLogAlertQuery {
			return e.executeLogAlertQuery(ctx, req)
		}

		queryType := model.Get("type").MustString("")

		var result *backend.QueryDataResponse // backend.Responses{}?
		switch queryType {
		case "metricFindQuery":
			result, _ = e.executeMetricFindQuery(ctx, model, q, req.PluginContext)
		case "annotationQuery":
			result, _ = e.executeAnnotationQuery(ctx, model, q, req.PluginContext)
		case "logAction":
			result, _ = e.executeLogActions(ctx, req)
		case "liveLogAction":
			result, _ = e.executeLiveLogQuery(ctx, req)
		case "timeSeriesQuery":
			fallthrough
		default:
			result, _ = e.executeTimeSeriesQuery(ctx, req)
		}

		if result != nil {
			for k, v := range result.Responses { // refactor?
				resp.Responses[k] = v
			}
		}
	}

	return resp, err // fix err
}

func (e *cloudWatchExecutor) executeLogAlertQuery(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	e.dsInstanceSettings = req.PluginContext.DataSourceInstanceSettings

	resp := backend.NewQueryDataResponse()

	for _, q := range req.Queries {
		model, err := simplejson.NewJson(q.JSON)
		if err != nil {
			continue
		}

		model.Set("subtype", "StartQuery")
		model.Set("queryString", model.Get("expression").MustString(""))

		region := model.Get("region").MustString(defaultRegion)
		if region == defaultRegion {
			var jsonData map[string]interface{}

			err := json.Unmarshal(req.PluginContext.DataSourceInstanceSettings.JSONData, &jsonData)
			if err != nil {
				return nil, err
			}
			model.Set("region", jsonData["defaultRegion"])
		}

		logsClient, err := e.getCWLogsClient(region, req.PluginContext)
		if err != nil {
			return nil, err
		}

		result, err := e.executeStartQuery(ctx, logsClient, model, q.TimeRange)
		if err != nil {
			return nil, err
		}

		model.Set("queryId", *result.QueryId)

		// Get query results
		getQueryResultsOutput, err := e.alertQuery(ctx, logsClient, q, model)
		if err != nil {
			return nil, err
		}

		dataframe, err := logsResultsToDataframes(getQueryResultsOutput)
		if err != nil {
			return nil, err
		}

		statsGroups := model.Get("statsGroups").MustStringArray()
		if len(statsGroups) > 0 && len(dataframe.Fields) > 0 {
			groupedFrames, err := groupResults(dataframe, statsGroups)
			if err != nil {
				return nil, err
			}

			respD := resp.Responses[q.RefID] // "A"?
			respD.Frames = groupedFrames
			resp.Responses[q.RefID] = respD
		} else {
			//?
			respD := resp.Responses[q.RefID] // "A"?
			respD.Frames = data.Frames{dataframe}
			resp.Responses[q.RefID] = respD
		}
	}

	return resp, nil
}

type authType int

const (
	authTypeDefault authType = iota
	authTypeSharedCreds
	authTypeKeys
)

func (at authType) String() string {
	switch at {
	case authTypeDefault:
		return "default"
	case authTypeSharedCreds:
		return "sharedCreds"
	case authTypeKeys:
		return "keys"
	default:
		panic(fmt.Sprintf("Unrecognized auth type %d", at))
	}
}

func (e *cloudWatchExecutor) getDSInfo(pluginCtx backend.PluginContext) *datasourceInfo {
	i, err := e.im.Get(pluginCtx)
	if err != nil {
		return nil
	}

	instance := i.(datasourceInfo)

	return &instance
}

func isTerminated(queryStatus string) bool {
	return queryStatus == "Complete" || queryStatus == "Cancelled" || queryStatus == "Failed" || queryStatus == "Timeout"
}

// NewCWClient is a CloudWatch client factory.
//
// Stubbable by tests.
var NewCWClient = func(sess *session.Session) cloudwatchiface.CloudWatchAPI {
	client := cloudwatch.New(sess)
	client.Handlers.Send.PushFront(func(r *request.Request) {
		r.HTTPRequest.Header.Set("User-Agent", fmt.Sprintf("Grafana/%s", setting.BuildVersion))
	})

	return client
}

// NewCWLogsClient is a CloudWatch logs client factory.
//
// Stubbable by tests.
var NewCWLogsClient = func(sess *session.Session) cloudwatchlogsiface.CloudWatchLogsAPI {
	client := cloudwatchlogs.New(sess)
	client.Handlers.Send.PushFront(func(r *request.Request) {
		r.HTTPRequest.Header.Set("User-Agent", fmt.Sprintf("Grafana/%s", setting.BuildVersion))
	})

	return client
}

// EC2 client factory.
//
// Stubbable by tests.
var newEC2Client = func(provider client.ConfigProvider) ec2iface.EC2API {
	return ec2.New(provider)
}

// RGTA client factory.
//
// Stubbable by tests.
var newRGTAClient = func(provider client.ConfigProvider) resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI {
	return resourcegroupstaggingapi.New(provider)
}
