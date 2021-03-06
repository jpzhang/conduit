package public

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/runconduit/conduit/controller/api/util"
	healthcheckPb "github.com/runconduit/conduit/controller/gen/common/healthcheck"
	tapPb "github.com/runconduit/conduit/controller/gen/controller/tap"
	telemPb "github.com/runconduit/conduit/controller/gen/controller/telemetry"
	pb "github.com/runconduit/conduit/controller/gen/public"
	"github.com/runconduit/conduit/pkg/version"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

type (
	grpcServer struct {
		telemetryClient telemPb.TelemetryClient
		tapClient       tapPb.TapClient
	}

	successRate struct {
		success float64
		failure float64
	}

	// sortable slice of unix ms timestamps
	timestamps []int64
)

const (
	countQuery                      = "sum(irate(responses_total{%s}[%s])) by (%s)"
	countHttpQuery                  = "sum(irate(http_requests_total{%s}[%s])) by (%s)"
	countGrpcQuery                  = "sum(irate(grpc_server_handled_total{%s}[%s])) by (%s)"
	latencyQuery                    = "sum(irate(response_latency_ms_bucket{%s}[%s])) by (%s)"
	quantileQuery                   = "histogram_quantile(%s, %s)"
	defaultVectorRange              = "30s" // 3x scrape_interval in prometheus config
	targetPodLabel                  = "target"
	targetDeployLabel               = "target_deployment"
	sourcePodLabel                  = "source"
	sourceDeployLabel               = "source_deployment"
	jobLabel                        = "job"
	TelemetryClientSubsystemName    = "telemetry"
	TelemetryClientCheckDescription = "control plane can use telemetry service"
)

var (
	quantileMap = map[string]pb.HistogramLabel{
		"0.5":  pb.HistogramLabel_P50,
		"0.95": pb.HistogramLabel_P95,
		"0.99": pb.HistogramLabel_P99,
	}

	stepMap = map[pb.TimeWindow]string{
		pb.TimeWindow_TEN_SEC:  "10s",
		pb.TimeWindow_ONE_MIN:  "10s",
		pb.TimeWindow_TEN_MIN:  "10s",
		pb.TimeWindow_ONE_HOUR: "1m",
	}

	aggregationMap = map[pb.AggregationType]string{
		pb.AggregationType_TARGET_DEPLOY: targetDeployLabel,
		pb.AggregationType_SOURCE_DEPLOY: sourceDeployLabel,
		pb.AggregationType_MESH:          jobLabel,
	}

	emptyMetadata = pb.MetricMetadata{}
)

func newGrpcServer(telemetryClient telemPb.TelemetryClient, tapClient tapPb.TapClient) *grpcServer {
	return &grpcServer{telemetryClient: telemetryClient, tapClient: tapClient}
}

func (s *grpcServer) Stat(ctx context.Context, req *pb.MetricRequest) (*pb.MetricResponse, error) {
	metrics := make([]*pb.MetricSeries, 0)

	for _, metric := range req.Metrics {
		var err error
		var series []*pb.MetricSeries

		switch metric {
		case pb.MetricName_REQUEST_RATE:
			if req.GroupBy == pb.AggregationType_MESH {
				series, err = s.requestRateMesh(ctx, req)
			} else {
				series, err = s.requestRate(ctx, req)
			}
		case pb.MetricName_SUCCESS_RATE:
			if req.GroupBy == pb.AggregationType_MESH {
				series, err = s.successRateMesh(ctx, req)
			} else {
				series, err = s.successRate(ctx, req)
			}
		case pb.MetricName_LATENCY:
			if req.GroupBy == pb.AggregationType_MESH {
				return nil, fmt.Errorf("latency not supported for MESH queries")
			} else {
				series, err = s.latency(ctx, req)
			}
		default:
			return nil, fmt.Errorf("unsupported metric: %s", metric)
		}

		if err != nil {
			return nil, err
		}
		metrics = append(metrics, series...)
	}

	return &pb.MetricResponse{Metrics: metrics}, nil
}

func (_ *grpcServer) Version(ctx context.Context, req *pb.Empty) (*pb.VersionInfo, error) {
	return &pb.VersionInfo{GoVersion: runtime.Version(), ReleaseVersion: version.Version, BuildDate: "1970-01-01T00:00:00Z"}, nil
}

func (s *grpcServer) ListPods(ctx context.Context, req *pb.Empty) (*pb.ListPodsResponse, error) {
	resp, err := s.telemetryClient.ListPods(ctx, &telemPb.ListPodsRequest{})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *grpcServer) SelfCheck(ctx context.Context, in *healthcheckPb.SelfCheckRequest) (*healthcheckPb.SelfCheckResponse, error) {
	telemetryClientCheck := &healthcheckPb.CheckResult{
		SubsystemName:    TelemetryClientSubsystemName,
		CheckDescription: TelemetryClientCheckDescription,
		Status:           healthcheckPb.CheckStatus_OK,
	}

	_, err := s.telemetryClient.ListPods(ctx, &telemPb.ListPodsRequest{})
	if err != nil {
		telemetryClientCheck.Status = healthcheckPb.CheckStatus_ERROR
		telemetryClientCheck.FriendlyMessageToUser = fmt.Sprintf("Error talking to telemetry service from control plane: %s", err.Error())
	}

	//TODO: check other services

	response := &healthcheckPb.SelfCheckResponse{
		Results: []*healthcheckPb.CheckResult{
			telemetryClientCheck,
		},
	}
	return response, nil
}

// Pass through to tap service
func (s *grpcServer) Tap(req *pb.TapRequest, stream pb.Api_TapServer) error {
	tapStream := stream.(tapServer)
	tapClient, err := s.tapClient.Tap(tapStream.Context(), req)
	if err != nil {
		//TODO: why not return the error?
		log.Errorf("Unexpected error tapping [%v]: %v", req, err)
		return nil
	}
	for {
		select {
		case <-tapStream.Context().Done():
			return nil
		default:
			event, err := tapClient.Recv()
			if err != nil {
				return err
			}
			tapStream.Send(event)
		}
	}
}

func (s *grpcServer) requestRate(ctx context.Context, req *pb.MetricRequest) ([]*pb.MetricSeries, error) {
	queryRsp, err := s.queryCount(ctx, req, countQuery, "")
	if err != nil {
		return nil, err
	}

	return processRequestRate(queryRsp.Metrics, extractMetadata)
}

func (s *grpcServer) requestRateMesh(ctx context.Context, req *pb.MetricRequest) ([]*pb.MetricSeries, error) {
	httpQueryRsp, err := s.queryCount(ctx, req, countHttpQuery, "")
	if err != nil {
		return nil, err
	}

	grpcQueryRsp, err := s.queryCount(ctx, req, countGrpcQuery, "")
	if err != nil {
		return nil, err
	}

	metrics := append(httpQueryRsp.Metrics, grpcQueryRsp.Metrics...)
	return processRequestRate(metrics, extractMetadataMesh)
}

func (s *grpcServer) successRate(ctx context.Context, req *pb.MetricRequest) ([]*pb.MetricSeries, error) {
	queryRsp, err := s.queryCount(ctx, req, countQuery, "classification")
	if err != nil {
		return nil, err
	}

	return processSuccessRate(queryRsp.Metrics, extractMetadata, isSuccess)
}

func (s *grpcServer) successRateMesh(ctx context.Context, req *pb.MetricRequest) ([]*pb.MetricSeries, error) {
	httpQueryRsp, err := s.queryCount(ctx, req, countHttpQuery, "code")
	if err != nil {
		return nil, err
	}

	grpcQueryRsp, err := s.queryCount(ctx, req, countGrpcQuery, "grpc_code")
	if err != nil {
		return nil, err
	}

	metrics := append(httpQueryRsp.Metrics, grpcQueryRsp.Metrics...)
	return processSuccessRate(metrics, extractMetadataMesh, isSuccessMesh)
}

func (s *grpcServer) latency(ctx context.Context, req *pb.MetricRequest) ([]*pb.MetricSeries, error) {
	timestamps := make(map[int64]struct{})
	latencies := make(map[pb.MetricMetadata]map[int64][]*pb.HistogramValue)
	series := make([]*pb.MetricSeries, 0)

	queryRsps, err := s.queryLatency(ctx, req)
	if err != nil {
		return nil, err
	}

	for label, queryRsp := range queryRsps {
		for _, metric := range queryRsp.Metrics {
			if len(metric.Values) == 0 {
				continue
			}

			metadata := extractMetadata(metric)
			if metadata == emptyMetadata {
				continue
			}

			if _, ok := latencies[metadata]; !ok {
				latencies[metadata] = make(map[int64][]*pb.HistogramValue)
			}

			for _, value := range metric.Values {
				if math.IsNaN(value.Value) {
					continue
				}
				timestamp := value.TimestampMs
				timestamps[timestamp] = struct{}{}

				if _, ok := latencies[metadata][timestamp]; !ok {
					latencies[metadata][timestamp] = make([]*pb.HistogramValue, 0)
				}
				hv := &pb.HistogramValue{
					Label: label,
					Value: int64(value.Value),
				}
				latencies[metadata][timestamp] = append(latencies[metadata][timestamp], hv)
			}
		}
	}

	sortedTimestamps := sortTimestamps(timestamps)

	for metadata, latenciesByTime := range latencies {
		m := metadata
		datapoints := make([]*pb.MetricDatapoint, 0)
		for _, ts := range sortedTimestamps {
			if histogram, ok := latenciesByTime[ts]; ok {
				datapoint := &pb.MetricDatapoint{
					Value: &pb.MetricValue{
						Value: &pb.MetricValue_Histogram{
							Histogram: &pb.Histogram{Values: histogram},
						},
					},
					TimestampMs: ts,
				}
				datapoints = append(datapoints, datapoint)
			}
		}

		s := &pb.MetricSeries{
			Name:       pb.MetricName_LATENCY,
			Metadata:   &m,
			Datapoints: datapoints,
		}
		series = append(series, s)
	}

	return series, nil
}

func (s *grpcServer) queryCount(ctx context.Context, req *pb.MetricRequest, rawQuery, sumBy string) (*telemPb.QueryResponse, error) {
	query, err := formatQuery(rawQuery, req, sumBy)
	if err != nil {
		return nil, err
	}

	queryRsp, err := s.query(ctx, req, query)
	if err != nil {
		return nil, err
	}

	return queryRsp, nil
}

// TODO: make these requests in parallel
func (s *grpcServer) queryLatency(ctx context.Context, req *pb.MetricRequest) (map[pb.HistogramLabel]*telemPb.QueryResponse, error) {
	queryRsps := make(map[pb.HistogramLabel]*telemPb.QueryResponse)

	query, err := formatQuery(latencyQuery, req, "le")
	if err != nil {
		return nil, err
	}

	for quantile, label := range quantileMap {
		q := fmt.Sprintf(quantileQuery, quantile, query)

		queryRsp, err := s.query(ctx, req, q)
		if err != nil {
			return nil, err
		}

		queryRsps[label] = queryRsp
	}

	return queryRsps, nil
}

func (s *grpcServer) query(ctx context.Context, req *pb.MetricRequest, query string) (*telemPb.QueryResponse, error) {
	queryReq := &telemPb.QueryRequest{
		Query: query,
	}

	if !req.Summarize {
		start, end, step, err := queryParams(req)
		if err != nil {
			return nil, err
		}

		queryReq.StartMs = start
		queryReq.EndMs = end
		queryReq.Step = step
	}

	queryRsp, err := s.telemetryClient.Query(ctx, queryReq)
	if err != nil {
		return nil, err
	}

	return queryRsp, nil
}

func formatQuery(query string, req *pb.MetricRequest, sumBy string) (string, error) {
	sumLabels := make([]string, 0)
	filterLabels := make([]string, 0)

	if str, ok := aggregationMap[req.GroupBy]; ok {
		sumLabels = append(sumLabels, str)
	} else {
		return "", fmt.Errorf("unsupported AggregationType")
	}
	if sumBy != "" {
		sumLabels = append(sumLabels, sumBy)
	}

	if metadata := req.FilterBy; metadata != nil {
		if metadata.TargetDeploy != "" {
			filterLabels = append(filterLabels, fmt.Sprintf("%s=\"%s\"", targetDeployLabel, metadata.TargetDeploy))
			sumLabels = append(sumLabels, targetDeployLabel)
		}
		if metadata.SourceDeploy != "" {
			filterLabels = append(filterLabels, fmt.Sprintf("%s=\"%s\"", sourceDeployLabel, metadata.SourceDeploy))
			sumLabels = append(sumLabels, sourceDeployLabel)
		}
		if metadata.Component != "" {
			filterLabels = append(filterLabels, fmt.Sprintf("%s=\"%s\"", jobLabel, metadata.Component))
			sumLabels = append(sumLabels, jobLabel)
		}
	}

	return fmt.Sprintf(
		query,
		strings.Join(filterLabels, ","),
		defaultVectorRange,
		strings.Join(sumLabels, ","),
	), nil
}

func queryParams(req *pb.MetricRequest) (int64, int64, string, error) {
	durationStr, err := util.GetWindowString(req.Window)
	if err != nil {
		return 0, 0, "", err
	}

	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		return 0, 0, "", err
	}

	end := time.Now()
	start := end.Add(-1 * duration)

	step, ok := stepMap[req.Window]
	if !ok {
		return 0, 0, "", fmt.Errorf("unsupported Window")
	}

	ms := int64(time.Millisecond)
	return start.UnixNano() / ms, end.UnixNano() / ms, step, nil
}

func extractMetadata(metric *telemPb.Sample) pb.MetricMetadata {
	return pb.MetricMetadata{
		TargetDeploy: metric.Labels[targetDeployLabel],
		SourceDeploy: metric.Labels[sourceDeployLabel],
	}
}

func extractMetadataMesh(metric *telemPb.Sample) pb.MetricMetadata {
	return pb.MetricMetadata{
		Component: metric.Labels[jobLabel],
	}
}

func isSuccess(labels map[string]string) bool {
	return labels["classification"] == "success"
}

func isSuccessMesh(labels map[string]string) (success bool) {
	// check to see if the http status code is anything but a 5xx error
	if v, ok := labels["code"]; ok && !strings.HasPrefix(v, "5") {
		success = true
	}
	// or check to see if the grpc status code is OK
	if v, ok := labels["grpc_code"]; ok && v == "OK" {
		success = true
	}
	return
}

func processRequestRate(
	metrics []*telemPb.Sample,
	metadataFn func(*telemPb.Sample) pb.MetricMetadata,
) ([]*pb.MetricSeries, error) {
	series := make([]*pb.MetricSeries, 0)

	for _, metric := range metrics {
		if len(metric.Values) == 0 {
			continue
		}

		datapoints := make([]*pb.MetricDatapoint, 0)
		for _, value := range metric.Values {
			if value.Value == 0 {
				continue
			}

			datapoint := pb.MetricDatapoint{
				Value: &pb.MetricValue{
					Value: &pb.MetricValue_Gauge{Gauge: value.Value},
				},
				TimestampMs: value.TimestampMs,
			}
			datapoints = append(datapoints, &datapoint)
		}

		metadata := metadataFn(metric)
		if metadata == emptyMetadata {
			continue
		}

		s := &pb.MetricSeries{
			Name:       pb.MetricName_REQUEST_RATE,
			Metadata:   &metadata,
			Datapoints: datapoints,
		}
		series = append(series, s)
	}

	return series, nil
}

func processSuccessRate(
	metrics []*telemPb.Sample,
	metadataFn func(*telemPb.Sample) pb.MetricMetadata,
	successRateFn func(map[string]string) bool,
) ([]*pb.MetricSeries, error) {
	timestamps := make(map[int64]struct{})
	successRates := make(map[pb.MetricMetadata]map[int64]*successRate)
	series := make([]*pb.MetricSeries, 0)

	for _, metric := range metrics {
		if len(metric.Values) == 0 {
			continue
		}

		isSuccess := successRateFn(metric.Labels)
		metadata := metadataFn(metric)
		if metadata == emptyMetadata {
			continue
		}

		if _, ok := successRates[metadata]; !ok {
			successRates[metadata] = make(map[int64]*successRate)
		}

		for _, value := range metric.Values {
			timestamp := value.TimestampMs
			timestamps[timestamp] = struct{}{}

			if _, ok := successRates[metadata][timestamp]; !ok {
				successRates[metadata][timestamp] = &successRate{}
			}

			if isSuccess {
				successRates[metadata][timestamp].success += value.Value
			} else {
				successRates[metadata][timestamp].failure += value.Value
			}
		}
	}

	sortedTimestamps := sortTimestamps(timestamps)

	for metadata, successRateByTime := range successRates {
		m := metadata
		datapoints := make([]*pb.MetricDatapoint, 0)
		for _, ts := range sortedTimestamps {
			if sr, ok := successRateByTime[ts]; ok {
				if requests := sr.success + sr.failure; requests > 0 {
					datapoint := &pb.MetricDatapoint{
						Value: &pb.MetricValue{
							Value: &pb.MetricValue_Gauge{Gauge: sr.success / requests},
						},
						TimestampMs: ts,
					}
					datapoints = append(datapoints, datapoint)
				}
			}
		}

		s := &pb.MetricSeries{
			Name:       pb.MetricName_SUCCESS_RATE,
			Metadata:   &m,
			Datapoints: datapoints,
		}
		series = append(series, s)
	}

	return series, nil
}

func (a timestamps) Len() int           { return len(a) }
func (a timestamps) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a timestamps) Less(i, j int) bool { return a[i] < a[j] }

func sortTimestamps(timestampMap map[int64]struct{}) timestamps {
	sorted := make(timestamps, len(timestampMap))
	for t, _ := range timestampMap {
		sorted = append(sorted, t)
	}
	sort.Sort(sorted)
	return sorted
}
