// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/model/value"
	prometheustranslator "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheus"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	conventions "go.opentelemetry.io/collector/semconv/v1.6.1"

	"github.com/grafana/mimir/pkg/mimirpb"
)

const (
	sumStr        = "_sum"
	countStr      = "_count"
	bucketStr     = "_bucket"
	leStr         = "le"
	quantileStr   = "quantile"
	pInfStr       = "+Inf"
	createdSuffix = "_created"
	// maxExemplarRunes is the maximum number of UTF-8 exemplar characters
	// according to the prometheus specification
	// https://github.com/OpenObservability/OpenMetrics/blob/main/specification/OpenMetrics.md#exemplars
	maxExemplarRunes = 128
	// Trace and Span id keys are defined as part of the spec:
	// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification%2Fmetrics%2Fdatamodel.md#exemplars-2
	traceIDKey       = "trace_id"
	spanIDKey        = "span_id"
	infoType         = "info"
	targetMetricName = "target_info"
)

type bucketBoundsData struct {
	sig   uint64
	bound float64
}

// byBucketBoundsData enables the usage of sort.Sort() with a slice of bucket bounds
type byBucketBoundsData []bucketBoundsData

func (m byBucketBoundsData) Len() int           { return len(m) }
func (m byBucketBoundsData) Less(i, j int) bool { return m[i].bound < m[j].bound }
func (m byBucketBoundsData) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }

// ByLabelName enables the usage of sort.Sort() with a slice of labels
type ByLabelName []mimirpb.LabelAdapter

func (a ByLabelName) Len() int           { return len(a) }
func (a ByLabelName) Less(i, j int) bool { return a[i].Name < a[j].Name }
func (a ByLabelName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// addSample finds a TimeSeries in tsMap that corresponds to the label set labels, and add sample to the TimeSeries; it
// creates a new TimeSeries in the map if not found and returns the time series signature.
// tsMap will be unmodified if either labels or sample is nil, but can still be modified if the exemplar is nil.
func addSample(tsMap map[uint64]*mimirpb.TimeSeries, sample *mimirpb.Sample, labels []mimirpb.LabelAdapter,
	datatype string) (uint64, error) {
	if sample == nil || len(labels) == 0 || len(tsMap) == 0 {
		// This shouldn't happen
		return 0, fmt.Errorf("invalid parameter")
	}

	sig, err := timeSeriesSignature(datatype, labels)
	if err != nil {
		return 0, err
	}
	ts := tsMap[sig]
	if ts != nil {
		ts.Samples = append(ts.Samples, *sample)
	} else {
		newTs := mimirpb.TimeseriesFromPool()
		newTs.Labels = labels
		newTs.Samples = append(newTs.Samples, *sample)
		tsMap[sig] = newTs
	}

	return sig, nil
}

// addExemplars finds a bucket bound that corresponds to the exemplars value and add the exemplar to the specific sig;
// we only add exemplars if samples are presents
// tsMap is unmodified if either of its parameters is nil and samples are nil.
func addExemplars(tsMap map[uint64]*mimirpb.TimeSeries, exemplars []mimirpb.Exemplar, bucketBoundsData []bucketBoundsData) {
	if len(tsMap) == 0 || len(bucketBoundsData) == 0 || len(exemplars) == 0 {
		return
	}

	sort.Sort(byBucketBoundsData(bucketBoundsData))

	for _, exemplar := range exemplars {
		addExemplar(tsMap, bucketBoundsData, exemplar)
	}
}

func addExemplar(tsMap map[uint64]*mimirpb.TimeSeries, bucketBounds []bucketBoundsData, exemplar mimirpb.Exemplar) {
	for _, bucketBound := range bucketBounds {
		sig := bucketBound.sig
		bound := bucketBound.bound

		ts := tsMap[sig]
		if ts != nil && len(ts.Samples) > 0 && exemplar.Value <= bound {
			ts.Exemplars = append(ts.Exemplars, exemplar)
			return
		}
	}
}

// timeSeries return a string signature in the form of:
//
//	TYPE-label1-value1- ...  -labelN-valueN
//
// the label slice should not contain duplicate label names; this method sorts the slice by label name before creating
// the signature.
func timeSeriesSignature(datatype string, labels []mimirpb.LabelAdapter) (uint64, error) {
	sort.Sort(ByLabelName(labels))

	h := xxhash.New()
	if _, err := h.WriteString(datatype); err != nil {
		return 0, err
	}
	if _, err := h.Write(seps); err != nil {
		return 0, err
	}
	for _, lb := range labels {
		if _, err := h.WriteString(lb.Name); err != nil {
			return 0, err
		}
		if _, err := h.Write(seps); err != nil {
			return 0, err
		}
		if _, err := h.WriteString(lb.Value); err != nil {
			return 0, err
		}
		if _, err := h.Write(seps); err != nil {
			return 0, err
		}
	}

	return h.Sum64(), nil
}

var seps = []byte{'\xff'}

// createAttributes creates a slice of Mimir labels with OTLP attributes and pairs of string values.
// Unpaired string values are ignored. String pairs overwrite OTLP labels if collisions happen, and overwrites are
// logged. Resulting label names are sanitized.
func createAttributes(resource pcommon.Resource, attributes pcommon.Map, externalLabels map[string]string, extras ...string) []mimirpb.LabelAdapter {
	serviceName, haveServiceName := resource.Attributes().Get(conventions.AttributeServiceName)
	instance, haveInstanceID := resource.Attributes().Get(conventions.AttributeServiceInstanceID)

	// Calculate the maximum possible number of labels we could return so we can preallocate l
	maxLabelCount := attributes.Len() + len(externalLabels) + len(extras)/2

	if haveServiceName {
		maxLabelCount++
	}

	if haveInstanceID {
		maxLabelCount++
	}

	// map ensures no duplicate label name
	l := make(map[string]string, maxLabelCount)

	// Ensure attributes are sorted by key for consistent merging of keys which
	// collide when sanitized.
	labels := make([]mimirpb.LabelAdapter, 0, attributes.Len())
	attributes.Range(func(key string, value pcommon.Value) bool {
		labels = append(labels, mimirpb.LabelAdapter{Name: key, Value: value.AsString()})
		return true
	})
	sort.Stable(ByLabelName(labels))

	for _, label := range labels {
		var finalKey = prometheustranslator.NormalizeLabel(label.Name)
		if existingValue, alreadyExists := l[finalKey]; alreadyExists {
			l[finalKey] = existingValue + ";" + label.Value
		} else {
			l[finalKey] = label.Value
		}
	}

	// Map service.name + service.namespace to job
	if haveServiceName {
		val := serviceName.AsString()
		if serviceNamespace, ok := resource.Attributes().Get(conventions.AttributeServiceNamespace); ok {
			val = fmt.Sprintf("%s/%s", serviceNamespace.AsString(), val)
		}
		l[model.JobLabel] = val
	}
	// Map service.instance.id to instance
	if haveInstanceID {
		l[model.InstanceLabel] = instance.AsString()
	}
	for key, value := range externalLabels {
		// External labels have already been sanitized
		if _, alreadyExists := l[key]; alreadyExists {
			// Skip external labels if they are overridden by metric attributes
			continue
		}
		l[key] = value
	}

	for i := 0; i < len(extras); i += 2 {
		if i+1 >= len(extras) {
			break
		}
		_, found := l[extras[i]]
		if found {
			log.Println("label " + extras[i] + " is overwritten. Check if Prometheus reserved labels are used.")
		}
		// internal labels should be maintained
		name := extras[i]
		if !(len(name) > 4 && name[:2] == "__" && name[len(name)-2:] == "__") {
			name = prometheustranslator.NormalizeLabel(name)
		}
		l[name] = extras[i+1]
	}

	s := make([]mimirpb.LabelAdapter, 0, len(l))
	for k, v := range l {
		s = append(s, mimirpb.LabelAdapter{Name: k, Value: v})
	}

	return s
}

// isValidAggregationTemporality checks whether an OTel metric has a valid
// aggregation temporality for conversion to a Mimir metric.
func isValidAggregationTemporality(metric pmetric.Metric) bool {
	//exhaustive:enforce
	switch metric.Type() {
	case pmetric.MetricTypeGauge, pmetric.MetricTypeSummary:
		return true
	case pmetric.MetricTypeSum:
		return metric.Sum().AggregationTemporality() == pmetric.AggregationTemporalityCumulative
	case pmetric.MetricTypeHistogram:
		return metric.Histogram().AggregationTemporality() == pmetric.AggregationTemporalityCumulative
	case pmetric.MetricTypeExponentialHistogram:
		return metric.ExponentialHistogram().AggregationTemporality() == pmetric.AggregationTemporalityCumulative
	}
	return false
}

// addSingleHistogramDataPoint converts pt to 2 + min(len(ExplicitBounds), len(BucketCount)) + 1 samples. It
// ignore extra buckets if len(ExplicitBounds) > len(BucketCounts)
func addSingleHistogramDataPoint(pt pmetric.HistogramDataPoint, resource pcommon.Resource, metric pmetric.Metric, settings Settings, tsMap map[uint64]*mimirpb.TimeSeries, baseName string) error {
	timestamp := convertTimeStamp(pt.Timestamp())
	baseLabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels)

	createLabels := func(nameSuffix string, extras ...string) []mimirpb.LabelAdapter {
		extraLabelCount := len(extras) / 2
		labels := make([]mimirpb.LabelAdapter, len(baseLabels), len(baseLabels)+extraLabelCount+1) // +1 for name
		copy(labels, baseLabels)

		for extrasIdx := 0; extrasIdx < extraLabelCount; extrasIdx++ {
			labels = append(labels, mimirpb.LabelAdapter{Name: extras[extrasIdx], Value: extras[extrasIdx+1]})
		}

		// sum, count, and buckets of the histogram should append suffix to baseName
		labels = append(labels, mimirpb.LabelAdapter{Name: model.MetricNameLabel, Value: baseName + nameSuffix})

		return labels
	}

	// If the sum is unset, it indicates the _sum metric point should be
	// omitted
	if pt.HasSum() {
		// treat sum as a sample in an individual TimeSeries
		sum := &mimirpb.Sample{
			Value:       pt.Sum(),
			TimestampMs: timestamp,
		}
		if pt.Flags().NoRecordedValue() {
			sum.Value = math.Float64frombits(value.StaleNaN)
		}

		sumlabels := createLabels(sumStr)
		if _, err := addSample(tsMap, sum, sumlabels, metric.Type().String()); err != nil {
			return err
		}

	}

	// treat count as a sample in an individual TimeSeries
	count := &mimirpb.Sample{
		Value:       float64(pt.Count()),
		TimestampMs: timestamp,
	}
	if pt.Flags().NoRecordedValue() {
		count.Value = math.Float64frombits(value.StaleNaN)
	}

	countlabels := createLabels(countStr)
	if _, err := addSample(tsMap, count, countlabels, metric.Type().String()); err != nil {
		return err
	}

	// cumulative count for conversion to cumulative histogram
	var cumulativeCount uint64

	exemplars := getMimirExemplars[pmetric.HistogramDataPoint](pt)

	var bucketBounds []bucketBoundsData

	// process each bound, based on histograms proto definition, # of buckets = # of explicit bounds + 1
	for i := 0; i < pt.ExplicitBounds().Len() && i < pt.BucketCounts().Len(); i++ {
		bound := pt.ExplicitBounds().At(i)
		cumulativeCount += pt.BucketCounts().At(i)
		bucket := &mimirpb.Sample{
			Value:       float64(cumulativeCount),
			TimestampMs: timestamp,
		}
		if pt.Flags().NoRecordedValue() {
			bucket.Value = math.Float64frombits(value.StaleNaN)
		}
		boundStr := strconv.FormatFloat(bound, 'f', -1, 64)
		labels := createLabels(bucketStr, leStr, boundStr)
		sig, err := addSample(tsMap, bucket, labels, metric.Type().String())
		if err != nil {
			return err
		}

		bucketBounds = append(bucketBounds, bucketBoundsData{sig: sig, bound: bound})
	}
	// add le=+Inf bucket
	infBucket := &mimirpb.Sample{
		TimestampMs: timestamp,
	}
	if pt.Flags().NoRecordedValue() {
		infBucket.Value = math.Float64frombits(value.StaleNaN)
	} else {
		infBucket.Value = float64(pt.Count())
	}
	infLabels := createLabels(bucketStr, leStr, pInfStr)
	sig, err := addSample(tsMap, infBucket, infLabels, metric.Type().String())
	if err != nil {
		return err
	}

	bucketBounds = append(bucketBounds, bucketBoundsData{sig: sig, bound: math.Inf(1)})
	addExemplars(tsMap, exemplars, bucketBounds)

	// add _created time series if needed
	startTimestamp := pt.StartTimestamp()
	if settings.ExportCreatedMetric && startTimestamp != 0 {
		labels := createLabels(createdSuffix)
		if err := addCreatedTimeSeriesIfNeeded(tsMap, labels, startTimestamp, pt.Timestamp(), metric.Type().String()); err != nil {
			return err
		}
	}

	return nil
}

type exemplarType interface {
	pmetric.ExponentialHistogramDataPoint | pmetric.HistogramDataPoint | pmetric.NumberDataPoint
	Exemplars() pmetric.ExemplarSlice
}

func getMimirExemplars[T exemplarType](pt T) []mimirpb.Exemplar {
	mimirExemplars := make([]mimirpb.Exemplar, 0, pt.Exemplars().Len())
	for i := 0; i < pt.Exemplars().Len(); i++ {
		exemplar := pt.Exemplars().At(i)
		exemplarRunes := 0

		mimirExemplar := mimirpb.Exemplar{
			Value:       exemplar.DoubleValue(),
			TimestampMs: timestamp.FromTime(exemplar.Timestamp().AsTime()),
		}
		if traceID := exemplar.TraceID(); !traceID.IsEmpty() {
			val := hex.EncodeToString(traceID[:])
			exemplarRunes += utf8.RuneCountInString(traceIDKey) + utf8.RuneCountInString(val)
			mimirLabel := mimirpb.LabelAdapter{
				Name:  traceIDKey,
				Value: val,
			}
			mimirExemplar.Labels = append(mimirExemplar.Labels, mimirLabel)
		}
		if spanID := exemplar.SpanID(); !spanID.IsEmpty() {
			val := hex.EncodeToString(spanID[:])
			exemplarRunes += utf8.RuneCountInString(spanIDKey) + utf8.RuneCountInString(val)
			mimirLabel := mimirpb.LabelAdapter{
				Name:  spanIDKey,
				Value: val,
			}
			mimirExemplar.Labels = append(mimirExemplar.Labels, mimirLabel)
		}

		attrs := exemplar.FilteredAttributes()
		labelsFromAttributes := make([]mimirpb.LabelAdapter, 0, attrs.Len())
		attrs.Range(func(key string, value pcommon.Value) bool {
			val := value.AsString()
			exemplarRunes += utf8.RuneCountInString(key) + utf8.RuneCountInString(val)
			mimirLabel := mimirpb.LabelAdapter{
				Name:  key,
				Value: val,
			}

			labelsFromAttributes = append(labelsFromAttributes, mimirLabel)

			return true
		})
		if exemplarRunes <= maxExemplarRunes {
			// only append filtered attributes if it does not cause exemplar
			// labels to exceed the max number of runes
			mimirExemplar.Labels = append(mimirExemplar.Labels, labelsFromAttributes...)
		}

		mimirExemplars = append(mimirExemplars, mimirExemplar)
	}

	return mimirExemplars
}

// mostRecentTimestampInMetric returns the latest timestamp in a batch of metrics
func mostRecentTimestampInMetric(metric pmetric.Metric) pcommon.Timestamp {
	var ts pcommon.Timestamp
	// handle individual metric based on type
	//exhaustive:enforce
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		dataPoints := metric.Gauge().DataPoints()
		for x := 0; x < dataPoints.Len(); x++ {
			ts = maxTimestamp(ts, dataPoints.At(x).Timestamp())
		}
	case pmetric.MetricTypeSum:
		dataPoints := metric.Sum().DataPoints()
		for x := 0; x < dataPoints.Len(); x++ {
			ts = maxTimestamp(ts, dataPoints.At(x).Timestamp())
		}
	case pmetric.MetricTypeHistogram:
		dataPoints := metric.Histogram().DataPoints()
		for x := 0; x < dataPoints.Len(); x++ {
			ts = maxTimestamp(ts, dataPoints.At(x).Timestamp())
		}
	case pmetric.MetricTypeExponentialHistogram:
		dataPoints := metric.ExponentialHistogram().DataPoints()
		for x := 0; x < dataPoints.Len(); x++ {
			ts = maxTimestamp(ts, dataPoints.At(x).Timestamp())
		}
	case pmetric.MetricTypeSummary:
		dataPoints := metric.Summary().DataPoints()
		for x := 0; x < dataPoints.Len(); x++ {
			ts = maxTimestamp(ts, dataPoints.At(x).Timestamp())
		}
	}
	return ts
}

func maxTimestamp(a, b pcommon.Timestamp) pcommon.Timestamp {
	if a > b {
		return a
	}
	return b
}

// addSingleSummaryDataPoint converts pt to len(QuantileValues) + 2 samples.
func addSingleSummaryDataPoint(pt pmetric.SummaryDataPoint, resource pcommon.Resource, metric pmetric.Metric, settings Settings,
	tsMap map[uint64]*mimirpb.TimeSeries, baseName string) error {
	timestamp := convertTimeStamp(pt.Timestamp())
	baseLabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels)

	createLabels := func(name string, extras ...string) []mimirpb.LabelAdapter {
		extraLabelCount := len(extras) / 2
		labels := make([]mimirpb.LabelAdapter, len(baseLabels), len(baseLabels)+extraLabelCount+1) // +1 for name
		copy(labels, baseLabels)

		for extrasIdx := 0; extrasIdx < extraLabelCount; extrasIdx++ {
			labels = append(labels, mimirpb.LabelAdapter{Name: extras[extrasIdx], Value: extras[extrasIdx+1]})
		}

		labels = append(labels, mimirpb.LabelAdapter{Name: model.MetricNameLabel, Value: name})

		return labels
	}

	// treat sum as a sample in an individual TimeSeries
	sum := &mimirpb.Sample{
		Value:       pt.Sum(),
		TimestampMs: timestamp,
	}
	if pt.Flags().NoRecordedValue() {
		sum.Value = math.Float64frombits(value.StaleNaN)
	}
	// sum and count of the summary should append suffix to baseName
	sumlabels := createLabels(baseName + sumStr)
	if _, err := addSample(tsMap, sum, sumlabels, metric.Type().String()); err != nil {
		return err
	}

	// treat count as a sample in an individual TimeSeries
	count := &mimirpb.Sample{
		Value:       float64(pt.Count()),
		TimestampMs: timestamp,
	}
	if pt.Flags().NoRecordedValue() {
		count.Value = math.Float64frombits(value.StaleNaN)
	}
	countlabels := createLabels(baseName + countStr)
	if _, err := addSample(tsMap, count, countlabels, metric.Type().String()); err != nil {
		return err
	}

	// process each percentile/quantile
	for i := 0; i < pt.QuantileValues().Len(); i++ {
		qt := pt.QuantileValues().At(i)
		quantile := &mimirpb.Sample{
			Value:       qt.Value(),
			TimestampMs: timestamp,
		}
		if pt.Flags().NoRecordedValue() {
			quantile.Value = math.Float64frombits(value.StaleNaN)
		}
		percentileStr := strconv.FormatFloat(qt.Quantile(), 'f', -1, 64)
		qtlabels := createLabels(baseName, quantileStr, percentileStr)
		if _, err := addSample(tsMap, quantile, qtlabels, metric.Type().String()); err != nil {
			return err
		}
	}

	// add _created time series if needed
	startTimestamp := pt.StartTimestamp()
	if settings.ExportCreatedMetric && startTimestamp != 0 {
		createdLabels := createLabels(baseName + createdSuffix)
		if err := addCreatedTimeSeriesIfNeeded(tsMap, createdLabels, startTimestamp, pt.Timestamp(), metric.Type().String()); err != nil {
			return err
		}
	}

	return nil
}

// addCreatedTimeSeriesIfNeeded adds {name}_created time series with a single
// sample. If the series exists, then new samples won't be added.
func addCreatedTimeSeriesIfNeeded(
	series map[uint64]*mimirpb.TimeSeries,
	labels []mimirpb.LabelAdapter,
	startTimestamp pcommon.Timestamp,
	timestamp pcommon.Timestamp,
	metricType string,
) error {
	sig, err := timeSeriesSignature(metricType, labels)
	if err != nil {
		return err
	}
	if _, ok := series[sig]; !ok {
		series[sig] = &mimirpb.TimeSeries{
			Labels: labels,
			Samples: []mimirpb.Sample{
				{ // convert ns to ms
					Value:       float64(convertTimeStamp(startTimestamp)),
					TimestampMs: convertTimeStamp(timestamp),
				},
			},
		}
	}

	return nil
}

// addResourceTargetInfo converts the resource to the target info metric
func addResourceTargetInfo(resource pcommon.Resource, settings Settings, timestamp pcommon.Timestamp, tsMap map[uint64]*mimirpb.TimeSeries) error {
	if settings.DisableTargetInfo {
		return nil
	}
	// Use resource attributes (other than those used for job+instance) as the
	// metric labels for the target info metric
	attributes := pcommon.NewMap()
	resource.Attributes().CopyTo(attributes)
	attributes.RemoveIf(func(k string, _ pcommon.Value) bool {
		switch k {
		case conventions.AttributeServiceName, conventions.AttributeServiceNamespace, conventions.AttributeServiceInstanceID:
			// Remove resource attributes used for job + instance
			return true
		default:
			return false
		}
	})
	if attributes.Len() == 0 {
		// If we only have job + instance, then target_info isn't useful, so don't add it.
		return nil
	}
	// create parameters for addSample
	name := targetMetricName
	if len(settings.Namespace) > 0 {
		name = settings.Namespace + "_" + name
	}
	labels := createAttributes(resource, attributes, settings.ExternalLabels, model.MetricNameLabel, name)
	sample := &mimirpb.Sample{
		Value: float64(1),
		// convert ns to ms
		TimestampMs: convertTimeStamp(timestamp),
	}
	_, err := addSample(tsMap, sample, labels, infoType)
	return err
}

// convertTimeStamp converts OTLP timestamp in ns to timestamp in ms
func convertTimeStamp(timestamp pcommon.Timestamp) int64 {
	return timestamp.AsTime().UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
}