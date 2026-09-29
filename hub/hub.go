/*
 * Copyright (c) Facebook, Inc. and its affiliates.
 *
 * This source code is licensed under the MIT license found in the
 * LICENSE file in the root directory of this source tree.
 */

package hub

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/golang/glog"
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	scrapeWorkerPoolSize = 100

	// ScraperQueryParam names the scraper on GET /metrics when the hub keeps
	// one buffer per scraper (see NewPerScraperMetricHub).
	ScraperQueryParam = "scraper"
)

var (
	hubLimit           = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hub_limit", Help: "Maximum number of datapoints in hub"})
	hubSize            = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hub_size", Help: "Number of datapoints in hub"})
	httpReceiveSizeFam = prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_receive_size_fam", Help: "Size of last HTTP receive (number of families)"})
	httpReceiveSizeDP  = prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_receive_size_dp", Help: "Size of last HTTP receive (number of datapoints)"})
	httpReceiveTime    = prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_receive_time", Help: "Time to ingest last HTTP receive"})
	parseTime          = prometheus.NewGauge(prometheus.GaugeOpts{Name: "parse_time", Help: "Time to parse last HTTP receive"})
	grpcReceiveSizeFam = prometheus.NewGauge(prometheus.GaugeOpts{Name: "grpc_receive_size_fam", Help: "Size of last GRPC receive (number of families)"})
	grpcReceiveSizeDP  = prometheus.NewGauge(prometheus.GaugeOpts{Name: "grpc_receive_size_dp", Help: "Size of last GRPC receive (number of datapoints)"})
	grpcReceiveTime    = prometheus.NewGauge(prometheus.GaugeOpts{Name: "grpc_receive_time", Help: "Time to ingest last GRPC receive"})

	scrapeLockWait = prometheus.NewGauge(prometheus.GaugeOpts{Name: "scrape_lock_wait", Help: "Time spent waiting on lock by last scrape request"})

	scraperDatapoints = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hub_scraper_datapoints",
		Help: "Datapoints waiting for one scraper in per-scraper mode",
	}, []string{"scraper"})
	scraperRejectedDatapoints = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hub_scraper_rejected_datapoints_total",
		Help: "Datapoints not retained for one scraper in per-scraper mode because its buffer was at the hub limit",
	}, []string{"scraper"})
)

func init() {
	prometheus.MustRegister(hubLimit, hubSize, httpReceiveSizeFam, httpReceiveSizeDP, httpReceiveTime, parseTime,
		grpcReceiveTime, grpcReceiveSizeDP, grpcReceiveSizeFam, scrapeLockWait,
		scraperDatapoints, scraperRejectedDatapoints)
}

// MetricHub serves as a replacement for the prometheus pushgateway. Accepts
// timestamps with metrics, and stores them in a queue to allow multiple
// datapoints per metric series to be scraped
type MetricHub struct {
	metricFamiliesByName map[string]*familyAndMetrics
	// perScraper is non-nil only for a hub built by NewPerScraperMetricHub.
	// It then replaces metricFamiliesByName: every push is retained once per
	// configured scraper, and a scrape drains only the requesting scraper's
	// buffer.
	perScraper map[string]*scraperBuffer
	limit      int
	stats      hubStats
	sync.Mutex
	scrapeTimeout int
}

// scraperBuffer holds the datapoints one scraper has not drained yet. The
// *dto.Metric values are shared by every buffer that accepted them and are
// never written after insertion, so buffers cost pointers, not copies.
type scraperBuffer struct {
	metricFamiliesByName map[string]*familyAndMetrics
	datapoints           int
}

// hubStats are for metrics that aren't worth exposing to prometheus, and also
// to provide a simpler way of exposing them in the `Debug` method since extracting
// values from the prometheus registry is not simple
type hubStats struct {
	lastScrapeTime        int64
	lastScrapeSize        int64
	lastScrapeNumFamilies int

	lastHTTPReceiveTime        int64
	lastHTTPReceiveSize        int64
	lastHTTPReceiveNumFamilies int

	lastGRPCReceiveTime        int64
	lastGRPCReceiveSize        int
	lastGRPCReceiveNumFamilies int

	currentCountFamilies   int
	currentCountSeries     int
	currentCountDatapoints int
}

func NewMetricHub(limit int, scrapeTimeout int) *MetricHub {
	if limit > 0 {
		glog.Infof("Prometheus-Edge-Hub created with a limit of %d\n", limit)
	} else {
		glog.Info("Prometheus-Edge-Hub created with no limit\n")
	}

	hubLimit.Set(float64(limit))

	return &MetricHub{
		metricFamiliesByName: make(map[string]*familyAndMetrics),
		limit:                limit,
		scrapeTimeout:        scrapeTimeout,
	}
}

// NewPerScraperMetricHub creates a hub that delivers every datapoint to each
// of the named scrapers exactly once.
//
// The single-buffer hub drains everything to whichever scraper calls first.
// That is correct for one Prometheus and wrong for a replicated one: N
// replicas scraping one hub each receive a disjoint ~1/N of every push, so
// no replica holds the full series and a query answered by one replica can
// be arbitrarily stale. Here each scraper names itself with
// ?scraper=<id> and drains only its own buffer.
//
// limit bounds each buffer independently. A buffer at the limit drops the
// push for that scraper only, counted in
// hub_scraper_rejected_datapoints_total, so one stalled replica cannot stop
// ingestion for the others. A push is refused outright only when no buffer
// can take it.
func NewPerScraperMetricHub(limit int, scrapeTimeout int, scrapers []string) (*MetricHub, error) {
	if len(scrapers) == 0 {
		return nil, fmt.Errorf("per-scraper hub needs at least one scraper id")
	}
	buffers := make(map[string]*scraperBuffer, len(scrapers))
	for _, id := range scrapers {
		if id == "" {
			return nil, fmt.Errorf("scraper ids must be non-empty: %q", scrapers)
		}
		if _, dup := buffers[id]; dup {
			return nil, fmt.Errorf("scraper id %q is listed more than once", id)
		}
		buffers[id] = &scraperBuffer{metricFamiliesByName: make(map[string]*familyAndMetrics)}
		scraperDatapoints.WithLabelValues(id).Set(0)
		scraperRejectedDatapoints.WithLabelValues(id).Add(0)
	}
	hub := NewMetricHub(limit, scrapeTimeout)
	hub.perScraper = buffers
	glog.Infof("Prometheus-Edge-Hub draining per scraper: %q\n", scrapers)
	return hub, nil
}

// receivePerScraper retains families in every scraper buffer with room for
// them and returns how many buffers accepted the push.
func (c *MetricHub) receivePerScraper(families []*dto.MetricFamily, newDatapoints int) int {
	// makeLabeledName sorts a metric's labels in place, and the same metric
	// is shared by every buffer, so name each datapoint once, up front.
	names := make([][]string, len(families))
	for i, fam := range families {
		names[i] = make([]string, len(fam.Metric))
		for j, metric := range fam.Metric {
			names[i][j] = makeLabeledName(metric, fam.GetName())
		}
	}

	accepted := 0
	largest := 0
	for id, buffer := range c.perScraper {
		if c.limit > 0 && buffer.datapoints+newDatapoints > c.limit {
			glog.Errorf("Not retaining push of size %d for scraper %q: would overfill hub limit of %d. Its buffer holds %d.\n",
				newDatapoints, id, c.limit, buffer.datapoints)
			scraperRejectedDatapoints.WithLabelValues(id).Add(float64(newDatapoints))
		} else {
			for i, fam := range families {
				stored, ok := buffer.metricFamiliesByName[fam.GetName()]
				if !ok {
					stored = &familyAndMetrics{family: familyHeader(fam), metrics: make(map[string][]*dto.Metric)}
					buffer.metricFamiliesByName[fam.GetName()] = stored
				}
				stored.insertNamed(names[i], fam.Metric)
			}
			buffer.datapoints += newDatapoints
			scraperDatapoints.WithLabelValues(id).Set(float64(buffer.datapoints))
			accepted++
		}
		if buffer.datapoints > largest {
			largest = buffer.datapoints
		}
	}
	// hub_size is compared against hub_limit, and here the limit applies per
	// buffer, so report the fullest one.
	c.stats.currentCountDatapoints = largest
	hubSize.Set(float64(largest))
	return accepted
}

// Receive is a handler function to receive metric pushes
func (c *MetricHub) Receive(ctx echo.Context) error {
	t0 := time.Now()
	var (
		err    error
		parser expfmt.TextParser
	)

	parsedFamilies, err := parser.TextToMetricFamilies(ctx.Request().Body)
	if err != nil {
		return ctx.String(http.StatusBadRequest, fmt.Sprintf("error parsing metrics: %v", err))
	}
	parseTime.Set(time.Since(t0).Seconds())

	newDatapoints := 0
	for _, fam := range parsedFamilies {
		newDatapoints += len(fam.Metric)
	}

	if c.perScraper != nil {
		families := make([]*dto.MetricFamily, 0, len(parsedFamilies))
		for _, fam := range parsedFamilies {
			families = append(families, fam)
		}
		c.Lock()
		accepted := c.receivePerScraper(families, newDatapoints)
		c.stats.lastHTTPReceiveTime = time.Now().Unix()
		c.stats.lastHTTPReceiveSize = ctx.Request().ContentLength
		c.stats.lastHTTPReceiveNumFamilies = len(parsedFamilies)
		c.Unlock()
		httpReceiveSizeDP.Set(float64(newDatapoints))
		httpReceiveSizeFam.Set(float64(len(parsedFamilies)))
		httpReceiveTime.Set(time.Since(t0).Seconds())
		if accepted == 0 {
			errString := fmt.Sprintf("Not accepting push of size %d. Every scraper buffer would overfill hub limit of %d.\n", newDatapoints, c.limit)
			return ctx.String(http.StatusNotAcceptable, errString)
		}
		return ctx.NoContent(http.StatusOK)
	}

	// Check if new datapoints will exceed the specified limit
	if c.limit > 0 {
		if c.stats.currentCountDatapoints+newDatapoints > c.limit {
			errString := fmt.Sprintf("Not accepting push of size %d. Would overfill hub limit of %d. Current hub size: %d\n", newDatapoints, c.limit, c.stats.currentCountDatapoints)
			glog.Error(errString)
			return ctx.String(http.StatusNotAcceptable, errString)
		}
	}
	httpReceiveSizeDP.Set(float64(newDatapoints))
	httpReceiveSizeFam.Set(float64(len(parsedFamilies)))

	t2 := time.Now()
	c.hubMetrics(parsedFamilies)
	httpReceiveTime.Set(time.Since(t2).Seconds())

	c.stats.lastHTTPReceiveTime = time.Now().Unix()
	c.stats.lastHTTPReceiveSize = ctx.Request().ContentLength
	c.stats.lastHTTPReceiveNumFamilies = len(parsedFamilies)
	c.stats.currentCountDatapoints += newDatapoints
	hubSize.Set(float64(c.stats.currentCountDatapoints))

	return ctx.NoContent(http.StatusOK)
}

func (c *MetricHub) hubMetrics(families map[string]*dto.MetricFamily) {
	c.Lock()
	defer c.Unlock()
	for _, fam := range families {
		if families, ok := c.metricFamiliesByName[fam.GetName()]; ok {
			families.addMetrics(fam.Metric)
		} else {
			c.metricFamiliesByName[fam.GetName()] = newFamilyAndMetrics(fam)
		}
	}
}

func (c *MetricHub) ReceiveGRPC(families []*dto.MetricFamily) {
	t0 := time.Now()

	c.Lock()
	defer c.Unlock()

	newDatapoints := 0
	for _, fam := range families {
		newDatapoints += len(fam.Metric)
	}

	if c.perScraper != nil {
		if c.receivePerScraper(families, newDatapoints) == 0 {
			return
		}
		grpcReceiveTime.Set(time.Since(t0).Seconds())
		grpcReceiveSizeFam.Set(float64(len(families)))
		grpcReceiveSizeDP.Set(float64(newDatapoints))
		c.stats.lastGRPCReceiveTime = time.Now().Unix()
		c.stats.lastGRPCReceiveNumFamilies = len(families)
		c.stats.lastGRPCReceiveSize = binary.Size(families)
		return
	}

	// Check if new datapoints will exceed the specified limit
	if c.limit > 0 {
		if c.stats.currentCountDatapoints+newDatapoints > c.limit {
			errString := fmt.Sprintf("Not accepting push of size %d. Would overfill hub limit of %d. Current hub size: %d\n", newDatapoints, c.limit, c.stats.currentCountDatapoints)
			glog.Error(errString)
			return
		}
	}

	for _, fam := range families {
		if families, ok := c.metricFamiliesByName[fam.GetName()]; ok {
			families.addMetrics(fam.Metric)
		} else {
			c.metricFamiliesByName[fam.GetName()] = newFamilyAndMetrics(fam)
		}
	}

	grpcReceiveTime.Set(time.Since(t0).Seconds())
	grpcReceiveSizeFam.Set(float64(len(families)))
	grpcReceiveSizeDP.Set(float64(newDatapoints))

	c.stats.lastGRPCReceiveTime = time.Now().Unix()
	c.stats.lastGRPCReceiveNumFamilies = len(families)
	c.stats.lastGRPCReceiveSize = binary.Size(families)
	c.stats.currentCountDatapoints += newDatapoints

}

// Scrape is a handler function for prometheus scrape requests. Formats the
// metrics for scraping.
func (c *MetricHub) Scrape(ctx echo.Context) error {
	scraper := ctx.QueryParam(ScraperQueryParam)
	if c.perScraper != nil {
		return c.scrapePerScraper(ctx, scraper)
	}
	if scraper != "" {
		// A scraper that names itself expects its own buffer. Draining the
		// shared one would silently take data from every other scraper, so
		// refuse and let the scrape error show the misconfiguration.
		return ctx.String(http.StatusBadRequest,
			fmt.Sprintf("%s=%q given, but this hub keeps a single shared buffer; start it with -scrapers to drain per scraper\n", ScraperQueryParam, scraper))
	}
	c.Lock()
	scrapeMetrics := c.metricFamiliesByName
	c.clearMetrics()
	c.Unlock()

	expositionString := c.exposeMetrics(scrapeMetrics, scrapeWorkerPoolSize)

	c.stats.lastScrapeTime = time.Now().Unix()
	c.stats.lastScrapeSize = int64(len(expositionString))
	c.stats.lastScrapeNumFamilies = len(scrapeMetrics)
	c.stats.currentCountDatapoints = 0
	hubSize.Set(0)

	return ctx.String(http.StatusOK, expositionString)
}

func (c *MetricHub) scrapePerScraper(ctx echo.Context, scraper string) error {
	c.Lock()
	buffer, ok := c.perScraper[scraper]
	if !ok {
		c.Unlock()
		return ctx.String(http.StatusBadRequest,
			fmt.Sprintf("%s=%q is not a configured scraper; this hub drains per scraper and requires one of its -scrapers ids\n", ScraperQueryParam, scraper))
	}
	scrapeMetrics := buffer.metricFamiliesByName
	buffer.metricFamiliesByName = make(map[string]*familyAndMetrics)
	buffer.datapoints = 0
	largest := 0
	for _, other := range c.perScraper {
		if other.datapoints > largest {
			largest = other.datapoints
		}
	}
	c.stats.currentCountDatapoints = largest
	c.Unlock()
	scraperDatapoints.WithLabelValues(scraper).Set(0)
	hubSize.Set(float64(largest))

	expositionString := c.exposeMetrics(scrapeMetrics, scrapeWorkerPoolSize)

	c.Lock()
	c.stats.lastScrapeTime = time.Now().Unix()
	c.stats.lastScrapeSize = int64(len(expositionString))
	c.stats.lastScrapeNumFamilies = len(scrapeMetrics)
	c.Unlock()

	return ctx.String(http.StatusOK, expositionString)
}

func (c *MetricHub) clearMetrics() {
	c.metricFamiliesByName = make(map[string]*familyAndMetrics)
}

func (c *MetricHub) exposeMetrics(metricFamiliesByName map[string]*familyAndMetrics, workers int) string {
	fams := make(chan *familyAndMetrics, workers)
	results := make(chan string, workers)
	respCh := make(chan string, 1)

	waitGroup := &sync.WaitGroup{}

	for i := 0; i < workers; i++ {
		waitGroup.Add(1)
		go processFamilyWorker(fams, results, waitGroup)
	}

	go processFamilyStringsWorker(results, respCh)

	for _, fam := range metricFamiliesByName {
		fams <- fam
	}

	close(fams)
	waitGroup.Wait()
	close(results)

	select {
	case resp := <-respCh:
		return resp
	case <-time.After(time.Duration(c.scrapeTimeout) * time.Second):
		log.Print("Timeout reached for building metrics string")
		return ""
	}
}

func processFamilyWorker(fams <-chan *familyAndMetrics, results chan<- string, waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()
	for fam := range fams {
		pullFamily := fam.popDatapoints()
		familyStr, err := familyToString(pullFamily)
		if err != nil {
			log.Printf("metric %s dropped. error converting metric to string: %v", *pullFamily.Name, err)
		} else {
			results <- familyStr
		}
	}
}

func processFamilyStringsWorker(results <-chan string, respCh chan<- string) {
	var resp strings.Builder
	for result := range results {
		resp.WriteString(result)
	}
	respCh <- resp.String()
}

// Debug is a handler function to show the current state of the hub without
// consuming any datapoints
func (c *MetricHub) Debug(ctx echo.Context) error {
	verbose := ctx.QueryParam("verbose")

	c.updateCountStats()
	hostname, _ := os.Hostname()
	var limitValue, utilizationValue string
	if c.limit <= 0 {
		limitValue = "None"
		utilizationValue = "0"
	} else {
		limitValue = strconv.Itoa(c.limit)
		utilizationValue = strconv.FormatFloat(float64(c.stats.currentCountDatapoints)*100/float64(c.limit), 'f', 2, 64)
	}

	debugString := fmt.Sprintf(`Prometheus Edge Hub running on %s
Hub Limit:       %s
Hub Utilization: %s%%

Last Scrape: %d
	Scrape Size: %d
	Number of Familes: %d

Last HTTP Receive: %d
	Receive Size: %d
	Number of Families: %d

Last GRPC Receive: %d
    Receive Size: %d
	Number of families: %d

Current Count Families:   %d
Current Count Series:     %d
Current Count Datapoints: %d `, hostname, limitValue, utilizationValue,
		c.stats.lastScrapeTime, c.stats.lastScrapeSize, c.stats.lastScrapeNumFamilies,
		c.stats.lastHTTPReceiveTime, c.stats.lastHTTPReceiveSize, c.stats.lastHTTPReceiveNumFamilies,
		c.stats.lastGRPCReceiveTime, c.stats.lastGRPCReceiveSize, c.stats.lastGRPCReceiveNumFamilies,
		c.stats.currentCountFamilies, c.stats.currentCountSeries, c.stats.currentCountDatapoints)

	if c.perScraper != nil {
		c.Lock()
		ids := make([]string, 0, len(c.perScraper))
		for id := range c.perScraper {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		debugString += "\n\nPer-scraper buffers:"
		for _, id := range ids {
			debugString += fmt.Sprintf("\n\t%s: %d datapoints", id, c.perScraper[id].datapoints)
		}
		c.Unlock()
		if verbose != "" {
			debugString += "\n\nVerbose exposition is not available in per-scraper mode; scrape /metrics?scraper=<id> instead.\n"
		}
	} else if verbose != "" {
		debugString += fmt.Sprintf("\n\nCurrent Exposition Text:\n%s\n", c.exposeMetrics(c.metricFamiliesByName, scrapeWorkerPoolSize))
	}

	return ctx.String(http.StatusOK, debugString)
}

func (c *MetricHub) updateCountStats() {
	numFamilies := len(c.metricFamiliesByName)
	numSeries := 0
	numDatapoints := 0
	for _, family := range c.metricFamiliesByName {
		numSeries += len(family.metrics)
		for _, series := range family.metrics {
			numDatapoints += len(series)
		}
	}
	c.stats.currentCountFamilies = numFamilies
	c.stats.currentCountSeries = numSeries
	c.stats.currentCountDatapoints = numDatapoints
}

type familyAndMetrics struct {
	family  *dto.MetricFamily
	metrics map[string][]*dto.Metric
}

func newFamilyAndMetrics(family *dto.MetricFamily) *familyAndMetrics {
	metrics := make(map[string][]*dto.Metric)
	for _, metric := range family.Metric {
		name := makeLabeledName(metric, family.GetName())
		if metricQueue, ok := metrics[name]; ok {
			if *metric.TimestampMs >= *metricQueue[len(metricQueue)-1].TimestampMs {
				metrics[name] = append(metricQueue, metric)
			} else {
				metrics[name] = sortedInsert(metricQueue, metric)
			}
		} else {
			metrics[name] = []*dto.Metric{metric}
		}
	}
	// clear metrics in family because we are keeping them in the queues
	family.Metric = nil

	return &familyAndMetrics{
		family:  family,
		metrics: metrics,
	}
}

func (f *familyAndMetrics) addMetrics(newMetrics []*dto.Metric) {
	// Keep array sorted [t0, t1, t2...] each insert
	for _, metric := range newMetrics {
		metricName := makeLabeledName(metric, f.family.GetName())
		if queue, ok := f.metrics[metricName]; ok {
			if *metric.TimestampMs >= *queue[len(queue)-1].TimestampMs {
				f.metrics[metricName] = append(queue, metric)
			} else {
				f.metrics[metricName] = sortedInsert(queue, metric)
			}
		} else {
			f.metrics[metricName] = []*dto.Metric{metric}
		}
	}
}

// insertNamed adds metrics whose labeled names were computed by the caller,
// keeping each series queue sorted by timestamp.
func (f *familyAndMetrics) insertNamed(names []string, newMetrics []*dto.Metric) {
	for i, metric := range newMetrics {
		name := names[i]
		if queue, ok := f.metrics[name]; ok {
			if *metric.TimestampMs >= *queue[len(queue)-1].TimestampMs {
				f.metrics[name] = append(queue, metric)
			} else {
				f.metrics[name] = sortedInsert(queue, metric)
			}
		} else {
			f.metrics[name] = []*dto.Metric{metric}
		}
	}
}

// familyHeader returns a family's name, help, type and unit without its
// metrics, so a buffer can own its header while sharing the datapoints.
func familyHeader(family *dto.MetricFamily) *dto.MetricFamily {
	return &dto.MetricFamily{
		Name: family.Name,
		Help: family.Help,
		Type: family.Type,
		Unit: family.Unit,
	}
}

// Returns a prometheus MetricFamily populated with all datapoints, sorted so
// that the earliest datapoint appears first
func (f *familyAndMetrics) popDatapoints() *dto.MetricFamily {
	// A fresh header rather than a value copy of f.family: a generated
	// message embeds protoimpl.MessageState, which must not be copied.
	pullFamily := familyHeader(f.family)
	// Series in name order, so a scrape's exposition does not depend on map
	// iteration order: every scraper of a per-scraper hub reads the same
	// text, and TestReceiveGRPCMultipleMetricsSameFamily stops flaking.
	names := make([]string, 0, len(f.metrics))
	for name := range f.metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		queue := f.metrics[name]
		if len(queue) == 0 {
			continue
		}
		pullFamily.Metric = append(pullFamily.Metric, queue...)
	}
	return pullFamily
}

// makeLabeledName builds a unique name from a metric LabelPairs
func makeLabeledName(metric *dto.Metric, metricName string) string {
	labels := metric.GetLabel()
	sort.Slice(labels, func(i, j int) bool {
		return labels[i].GetName() < labels[j].GetName()
	})

	labeledName := strings.Builder{}
	labeledName.WriteString(metricName)
	for _, labelPair := range labels {
		labeledName.WriteString(fmt.Sprintf("_%s_%s", labelPair.GetName(), labelPair.GetValue()))
	}
	return labeledName.String()
}

func familyToString(family *dto.MetricFamily) (string, error) {
	var buf bytes.Buffer
	_, err := expfmt.MetricFamilyToText(&buf, family)
	if err != nil {
		return "", fmt.Errorf("error writing family string: %v", err)
	}
	return buf.String(), nil
}

func WriteInternalMetrics() (string, error) {
	metrics, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return "", err
	}
	str := strings.Builder{}
	for _, fam := range metrics {
		buf := bytes.Buffer{}
		_, err := expfmt.MetricFamilyToText(&buf, fam)
		if err != nil {
			return "", err
		}
		str.WriteString(buf.String())
	}
	return str.String(), nil
}

func sortedInsert(data []*dto.Metric, el *dto.Metric) []*dto.Metric {
	index := sort.Search(len(data), func(i int) bool { return *data[i].TimestampMs > *el.TimestampMs })
	data = append(data, &dto.Metric{})
	copy(data[index+1:], data[index:])
	data[index] = el
	return data
}
