package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/matttproud/golang_protobuf_extensions/pbutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"time"
)

const (
	batchSize      = 10
	cwHighResLabel = "__cw_high_res"
	cwUnitLabel    = "__cw_unit"
	acceptHeader   = `application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.7,text/plain;version=0.0.4;q=0.3`
)

// Config defines configuration options
type Config struct {
	// AWS access key Id with permissions to publish CloudWatch metrics
	AwsAccessKeyId string

	// AWS secret access key with permissions to publish CloudWatch metrics
	AwsSecretAccessKey string

	// Required. The CloudWatch namespace under which metrics should be published
	CloudWatchNamespace string

	// Required. The AWS Region to use
	CloudWatchRegion string

	// The frequency with which metrics should be published to Cloudwatch. Default: 15s
	CloudWatchPublishInterval time.Duration

	// Timeout for sending metrics to Cloudwatch. Default: 3s
	CloudWatchPublishTimeout time.Duration

	// Prometheus scrape URL
	PrometheusScrapeUrl string

	// Path to Certificate file
	PrometheusCertPath string

	// Path to Key file
	PrometheusKeyPath string

	// Accept any certificate during TLS handshake. Insecure, use only for testing
	PrometheusSkipServerCertCheck bool

	// Additional dimensions to send to CloudWatch
	AdditionalDimensions map[string]string

	// Replace dimensions with the provided label. This allows for aggregating metrics across dimensions so we can set CloudWatch Alarms on the metrics
	ReplaceDimensions map[string]string
}

// Bridge pushes metrics to AWS CloudWatch
type Bridge struct {
	cloudWatchPublishInterval     time.Duration
	cloudWatchNamespace           string
	cw                            *cloudwatch.CloudWatch
	prometheusScrapeUrl           string
	prometheusCertPath            string
	prometheusKeyPath             string
	prometheusSkipServerCertCheck bool
	additionalDimensions          map[string]string
	replaceDimensions             map[string]string
	metricNameWhitelistRegex      string
	replaceDimensionsRegex        map[string]map[string]string
}

// NewBridge initializes and returns a pointer to a Bridge using the
// supplied configuration, or an error if there is a problem with the configuration
func NewBridge(c *Config) (*Bridge, error) {
	b := &Bridge{}

	if c.CloudWatchNamespace == "" {
		return nil, errors.New("CloudWatchNamespace required")
	}
	b.cloudWatchNamespace = c.CloudWatchNamespace

	if c.PrometheusScrapeUrl == "" {
		return nil, errors.New("PrometheusScrapeUrl required")
	}
	b.prometheusScrapeUrl = c.PrometheusScrapeUrl

	b.prometheusCertPath = c.PrometheusCertPath
	b.prometheusKeyPath = c.PrometheusKeyPath
	b.prometheusSkipServerCertCheck = c.PrometheusSkipServerCertCheck
	b.additionalDimensions = c.AdditionalDimensions
	b.replaceDimensions = c.ReplaceDimensions
	b.metricNameWhitelistRegex = "^([a-z,_]*)(kube_node_status_condition|kube_endpoint_address_available|kube_pod_container_status_terminated_reason|kube_pod_container_status_waiting_reason|kube_pod_status_ready)([a-z,_]*)$"
	b.replaceDimensionsRegex = make(map[string]map[string]string)
	replaceEndpointRegex := make(map[string]string)
	replaceEndpointRegex["^(insight-engine-service-)([a-z,0-9,_]*)$"] = "insight-engine-service"
	replaceEndpointRegex["^(notebook-insight-engine-service-)([a-z,0-9,_]*)$"] = "notebook-insight-engine-service"
	replaceEndpointRegex["^(ingress-insight-engine-)([a-z,0-9,_]*)$"] = "insight-engine-service"
	replaceEndpointRegex["^(ingress-insight-notebook-)([a-z,0-9,_]*)$"] = "notebook-insight-engine-service"
	replaceEndpointRegex["^(insight-engine-)([a-z,0-9,_]*)$"] = "insight-engine-service"
	b.replaceDimensionsRegex["endpoint"] = replaceEndpointRegex
	b.replaceDimensionsRegex["service"] = replaceEndpointRegex

	if c.CloudWatchPublishInterval > 0 {
		b.cloudWatchPublishInterval = c.CloudWatchPublishInterval
	} else {
		b.cloudWatchPublishInterval = 30 * time.Second
	}

	var client = http.DefaultClient

	if c.CloudWatchPublishTimeout > 0 {
		client.Timeout = c.CloudWatchPublishTimeout
	} else {
		client.Timeout = 5 * time.Second
	}

	if c.CloudWatchRegion == "" {
		return nil, errors.New("CloudWatchRegion required")
	}

	config := aws.NewConfig().WithHTTPClient(client).WithRegion(c.CloudWatchRegion)

	// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html
	// https://docs.aws.amazon.com/sdk-for-go/api/aws/#Config
	// If credentials are not provided in the variables, the chain of credential providers will search for credentials
	// in environment variables, the shared credential file, and EC2 Instance Roles
	if c.AwsAccessKeyId != "" && c.AwsSecretAccessKey != "" {
		config.Credentials = credentials.NewStaticCredentials(c.AwsAccessKeyId, c.AwsSecretAccessKey, "")
	}

	sess, err := session.NewSession(config)
	if err != nil {
		return nil, err
	}

	b.cw = cloudwatch.New(sess)
	return b, nil
}

// Run starts a loop that will push metrics to Cloudwatch at the configured interval. Accepts a context.Context to support cancellation
func (b *Bridge) Run(ctx context.Context) {
	ticker := time.NewTicker(b.cloudWatchPublishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mfChan := make(chan *dto.MetricFamily, 1024)

			go fetchMetricFamilies(b.prometheusScrapeUrl, mfChan, b.prometheusCertPath, b.prometheusKeyPath, b.prometheusSkipServerCertCheck)

			var metricFamilies []*dto.MetricFamily
			for mf := range mfChan {
				metricFamilies = append(metricFamilies, mf)
			}

			err := b.publishMetricsToCloudWatch(metricFamilies)
			if err != nil {
				log.Println("prometheus-to-cloudwatch: error publishing to CloudWatch:", err)
			} else {
				log.Println(fmt.Sprintf("prometheus-to-cloudwatch: published %d metrics to CloudWatch", len(metricFamilies)))
			}

		case <-ctx.Done():
			log.Println("prometheus-to-cloudwatch: stopping")
			return
		}
	}
}

// NOTE: The CloudWatch API has the following limitations:
//  - Max 40kb request size
//	- Single namespace per request
//	- Max 10 dimensions per metric
func (b *Bridge) publishMetricsToCloudWatch(mfs []*dto.MetricFamily) error {
	vec, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{Timestamp: model.Now()}, mfs...)

	if err != nil {
		return err
	}

	data := make([]*cloudwatch.MetricDatum, 0, batchSize)

	for _, s := range vec {
		name := getName(s.Metric)
		whitelistPattern, _ := regexp.Compile(b.metricNameWhitelistRegex)
		if whitelistPattern.MatchString(name) {
			data = appendDatum(data, name, s, b)
			if len(data) == batchSize {
				if err := b.flush(data); err != nil {
					log.Println("prometheus-to-cloudwatch: error publishing to CloudWatch:", err)
				}
				data = make([]*cloudwatch.MetricDatum, 0, batchSize)
			}
		}
	}

	return b.flush(data)
}

func (b *Bridge) flush(data []*cloudwatch.MetricDatum) error {
	if len(data) > 0 {
		in := &cloudwatch.PutMetricDataInput{
			MetricData: data,
			Namespace:  &b.cloudWatchNamespace,
		}
		_, err := b.cw.PutMetricData(in)
		return err
	}
	return nil
}

func appendDatum(data []*cloudwatch.MetricDatum, name string, s *model.Sample, b *Bridge) []*cloudwatch.MetricDatum {
	metric := s.Metric

	if len(metric) == 0 {
		return data
	}

	datum := &cloudwatch.MetricDatum{}

	kubeStateDimensions, replacedDimensions := getDimensions(metric, 10-len(b.additionalDimensions), b)

	// Original dimension is not published if replacement dimension is found
	if replacedDimensions == nil && len(replacedDimensions) == 0 {
		datum.SetMetricName(name).
			SetValue(float64(s.Value)).
			SetTimestamp(s.Timestamp.Time()).
			SetDimensions(append(kubeStateDimensions, getAdditionalDimensions(b)...)).
			SetStorageResolution(getResolution(metric)).
			SetUnit(getUnit(metric))
		data = append(data, datum)
	}

	// Don't add replacement if not configured
	if replacedDimensions != nil && len(replacedDimensions) > 0 {
		replacedDimensionDatum := &cloudwatch.MetricDatum{}
		replacedDimensionDatum.SetMetricName(name).
			SetValue(float64(s.Value)).
			SetTimestamp(s.Timestamp.Time()).
			SetDimensions(append(replacedDimensions, getAdditionalDimensions(b)...)).
			SetStorageResolution(getResolution(metric)).
			SetUnit(getUnit(metric))
		data = append(data, replacedDimensionDatum)
	}

	return data
}

func getName(m model.Metric) string {
	if n, ok := m[model.MetricNameLabel]; ok {
		return string(n)
	}
	return ""
}

// getDimensions returns up to 10 dimensions for the provided metric - one for each label (except the __name__ label)
// If a metric has more than 10 labels, it attempts to behave deterministically and returning the first 10 labels as dimensions
func getDimensions(m model.Metric, num int, b *Bridge) ([]*cloudwatch.Dimension, []*cloudwatch.Dimension) {
	if len(m) == 0 {
		return make([]*cloudwatch.Dimension, 0), nil
	} else if _, ok := m[model.MetricNameLabel]; len(m) == 1 && ok {
		return make([]*cloudwatch.Dimension, 0), nil
	}

	names := make([]string, 0, len(m))
	for k := range m {
		if !(k == model.MetricNameLabel || k == cwHighResLabel || k == cwUnitLabel) {
			names = append(names, string(k))
		}
	}

	sort.Strings(names)
	dims := make([]*cloudwatch.Dimension, 0, len(names))
	replacedDims := make([]*cloudwatch.Dimension, 0, len(names))

	for _, name := range names {
		if name != "" {
			val := string(m[model.LabelName(name)])
			if val != "" {
				dims = append(dims, new(cloudwatch.Dimension).SetName(name).SetValue(val))

				wasReplaced := false
				// Don't add replacement if not configured
				if b.replaceDimensions != nil && len(b.replaceDimensions) > 0 {
					if replacement, ok := b.replaceDimensions[name]; ok {
						replacedDims = append(replacedDims, new(cloudwatch.Dimension).SetName(name).SetValue(replacement))
						wasReplaced = true
					} else if b.replaceDimensionsRegex != nil && len(b.replaceDimensionsRegex) > 0 {
						if expr, ok := b.replaceDimensionsRegex[name]; ok {
							for k, v := range expr {
								// TODO pre-compile the regex
								if matchPattern, err := regexp.Compile(k); err == nil {
									if matchPattern.MatchString(val) {
										//log.Println(fmt.Sprintf("prometheus-to-cloudwatch: Replacing dimensions. Name %s, original value %s, new values %s, replaceDimensionsRegex key %s", name, val, v, k))
										replacedDims = append(replacedDims, new(cloudwatch.Dimension).SetName(name).SetValue(v))
										wasReplaced = true
									}
								} else {
									log.Println(fmt.Sprintf("prometheus-to-cloudwatch: Error when compiling regex %s. Err %v", k, err))
								}
							}
						}
					}
				}
				if !wasReplaced {
					replacedDims = append(replacedDims, new(cloudwatch.Dimension).SetName(name).SetValue(val))
				}
			}
		}
	}

	if len(dims) > num {
		dims = dims[:num]
	}

	if len(replacedDims) > num {
		replacedDims = replacedDims[:num]
	}

	return dims, replacedDims
}

func getAdditionalDimensions(b *Bridge) []*cloudwatch.Dimension {
	dims := make([]*cloudwatch.Dimension, 0, len(b.additionalDimensions))
	for k, v := range b.additionalDimensions {
		dims = append(dims, new(cloudwatch.Dimension).SetName(k).SetValue(v))
	}
	return dims
}

// Returns 1 if the metric contains a __cw_high_res label, otherwise returns 60
func getResolution(m model.Metric) int64 {
	if _, ok := m[cwHighResLabel]; ok {
		return 1
	}
	return 60
}

func getUnit(m model.Metric) string {
	if u, ok := m[cwUnitLabel]; ok {
		return string(u)
	}
	return "None"
}

// fetchMetricFamilies retrieves metrics from the provided URL, decodes them into MetricFamily proto messages, and sends them to the provided channel.
// It returns after all MetricFamilies have been sent
func fetchMetricFamilies(
	url string, ch chan<- *dto.MetricFamily,
	certificate string, key string,
	skipServerCertCheck bool,
) {
	defer close(ch)
	var transport *http.Transport
	if certificate != "" && key != "" {
		cert, err := tls.LoadX509KeyPair(certificate, key)
		if err != nil {
			log.Fatal("prometheus-to-cloudwatch: Error: ", err)
		}
		tlsConfig := &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: skipServerCertCheck,
		}
		tlsConfig.BuildNameToCertificate()
		transport = &http.Transport{TLSClientConfig: tlsConfig}
	} else {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipServerCertCheck},
		}
	}
	client := &http.Client{Transport: transport, Timeout: time.Second * 15}
	decodeContent(client, url, ch)
}

func decodeContent(client *http.Client, url string, ch chan<- *dto.MetricFamily) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatalf("prometheus-to-cloudwatch: Error: creating GET request for URL %q failed: %s", url, err)
	}
	req.Header.Add("Accept", acceptHeader)
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("prometheus-to-cloudwatch: Error: executing GET request for URL %q failed: %s", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("prometheus-to-cloudwatch: Error: GET request for URL %q returned HTTP status %s", url, resp.Status)
	}
	parseResponse(resp, ch)
}

// parseResponse consumes an http.Response and pushes it to the channel.
// It returns when all all MetricFamilies are parsed and put on the channel.
func parseResponse(resp *http.Response, ch chan<- *dto.MetricFamily) {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))

	if err == nil && mediaType == "application/vnd.google.protobuf" && params["encoding"] == "delimited" && params["proto"] == "io.prometheus.client.MetricFamily" {
		for {
			mf := &dto.MetricFamily{}
			if _, err = pbutil.ReadDelimited(resp.Body, mf); err != nil {
				if err == io.EOF {
					break
				}
				log.Fatalln("prometheus-to-cloudwatch: Error: reading metric family protocol buffer failed:", err)
			}
			ch <- mf
		}
	} else {
		var parser expfmt.TextParser
		metricFamilies, err := parser.TextToMetricFamilies(resp.Body)
		if err != nil {
			log.Fatalln("reading text format failed:", err)
		}
		for _, mf := range metricFamilies {
			ch <- mf
		}
	}
}
