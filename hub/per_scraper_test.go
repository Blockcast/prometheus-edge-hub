/*
 * Copyright (c) Facebook, Inc. and its affiliates.
 *
 * This source code is licensed under the MIT license found in the
 * LICENSE file in the root directory of this source tree.
 */

package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleReceiveString carries 14 datapoints in 3 families.
const sampleDatapoints = 14

func scrapeAs(t *testing.T, hub *MetricHub, scraper string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/metrics"
	if scraper != "" {
		target += "?" + ScraperQueryParam + "=" + scraper
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	require.NoError(t, hub.Scrape(echo.New().NewContext(req, rec)))
	return rec
}

func rejectedFor(t *testing.T, scraper string) float64 {
	t.Helper()
	metric := &dto.Metric{}
	require.NoError(t, scraperRejectedDatapoints.WithLabelValues(scraper).Write(metric))
	return metric.GetCounter().GetValue()
}

func datapointsIn(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(rec.Body)
	require.NoError(t, err)
	total := 0
	for _, family := range families {
		total += len(family.Metric)
	}
	return total
}

// The defect, reproduced on the single-buffer hub: the first scrape after a
// push takes all of it, so a second replica scraping the same hub gets
// nothing. Across a replicated Prometheus that splits every series into
// disjoint halves (BLO-35597 measured 3 vs 11 samples of one series on the
// two orc8r-prometheus replicas over 20 minutes).
func TestSingleBufferHubSplitsPushesAcrossScrapers(t *testing.T) {
	hub := NewMetricHub(0, 10)
	_, err := receiveString(hub, sampleReceiveString)
	require.NoError(t, err)

	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "")))
	assert.Equal(t, 0, datapointsIn(t, scrapeAs(t, hub, "")), "the second scraper sees none of the push")
}

func TestPerScraperHubDeliversEveryPushToEveryScraperOnce(t *testing.T) {
	hub, err := NewPerScraperMetricHub(0, 10, []string{"orc8r-prometheus-0", "orc8r-prometheus-1"})
	require.NoError(t, err)
	resp, err := receiveString(hub, sampleReceiveString)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Code)

	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "orc8r-prometheus-0")))
	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "orc8r-prometheus-1")),
		"a second replica receives the same push")
	assert.Equal(t, 0, datapointsIn(t, scrapeAs(t, hub, "orc8r-prometheus-0")),
		"each scraper drains its own copy exactly once")
}

func TestPerScraperHubDeliversGRPCPushesToEveryScraper(t *testing.T) {
	hub, err := NewPerScraperMetricHub(0, 10, []string{"a", "b"})
	require.NoError(t, err)
	hub.ReceiveGRPC([]*dto.MetricFamily{
		makeFamily(dto.MetricType_GAUGE, "fam1", 10, []*dto.LabelPair{}, 1),
		makeFamily(dto.MetricType_GAUGE, "fam2", 10, []*dto.LabelPair{}, 1),
	})

	assert.Equal(t, 20, datapointsIn(t, scrapeAs(t, hub, "a")))
	assert.Equal(t, 20, datapointsIn(t, scrapeAs(t, hub, "b")))
}

// Identical datapoints for each scraper, in the same timestamp order within
// every series: the buffers share datapoints but must not share queue state.
// Families are compared by name, because exposeMetrics renders them on a
// worker pool and concatenates them in completion order.
func TestPerScraperHubServesIdenticalFamiliesToEachScraper(t *testing.T) {
	hub, err := NewPerScraperMetricHub(0, 10, []string{"a", "b"})
	require.NoError(t, err)
	_, err = receiveString(hub, sampleReceiveString)
	require.NoError(t, err)

	a := scrapeAs(t, hub, "a")
	b := scrapeAs(t, hub, "b")
	require.Equal(t, http.StatusOK, a.Code)
	require.Equal(t, http.StatusOK, b.Code)
	var parser expfmt.TextParser
	aFamilies, err := parser.TextToMetricFamilies(a.Body)
	require.NoError(t, err)
	bFamilies, err := parser.TextToMetricFamilies(b.Body)
	require.NoError(t, err)
	require.Len(t, bFamilies, len(aFamilies))
	require.NotEmpty(t, aFamilies)
	for name, family := range aFamilies {
		other, ok := bFamilies[name]
		require.True(t, ok, "family %s missing for scraper b", name)
		assert.Equal(t, family.String(), other.String(), "family %s differs between scrapers", name)
	}
}

// A replica that stops scraping fills only its own buffer. The healthy one
// keeps receiving, and the stalled one's losses are counted, not silent.
func TestPerScraperLimitIsPerScraper(t *testing.T) {
	hub, err := NewPerScraperMetricHub(sampleDatapoints, 10, []string{"healthy", "stalled"})
	require.NoError(t, err)
	stalledRejected := rejectedFor(t, "stalled")

	resp, err := receiveString(hub, sampleReceiveString)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Code)
	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "healthy")))

	// "stalled" still holds the first push, so the second fits only "healthy".
	resp, err = receiveString(hub, sampleReceiveString)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.Code, "a push any buffer can take is accepted")
	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "healthy")))
	assert.Equal(t, float64(sampleDatapoints),
		rejectedFor(t, "stalled")-stalledRejected)
	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "stalled")),
		"the stalled scraper keeps what it had room for")
}

func TestPerScraperHubRefusesAPushNoBufferCanTake(t *testing.T) {
	hub, err := NewPerScraperMetricHub(sampleDatapoints, 10, []string{"a", "b"})
	require.NoError(t, err)
	resp, err := receiveString(hub, sampleReceiveString)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Code)

	resp, err = receiveString(hub, sampleReceiveString)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotAcceptable, resp.Code)
}

// A per-scraper hub cannot guess who is asking: an unnamed or unknown scraper
// would otherwise drain someone else's buffer, which is the defect again.
func TestPerScraperHubRejectsUnnamedAndUnknownScrapers(t *testing.T) {
	hub, err := NewPerScraperMetricHub(0, 10, []string{"a"})
	require.NoError(t, err)
	_, err = receiveString(hub, sampleReceiveString)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadRequest, scrapeAs(t, hub, "").Code)
	assert.Equal(t, http.StatusBadRequest, scrapeAs(t, hub, "b").Code)
	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "a")),
		"refused scrapes must not drain anything")
}

// A scraper configured for per-scraper draining against a hub that was not
// started that way fails its scrape instead of silently draining the shared
// buffer out from under every other scraper.
func TestSingleBufferHubRejectsANamedScraper(t *testing.T) {
	hub := NewMetricHub(0, 10)
	_, err := receiveString(hub, sampleReceiveString)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadRequest, scrapeAs(t, hub, "orc8r-prometheus-0").Code)
	assert.Equal(t, sampleDatapoints, datapointsIn(t, scrapeAs(t, hub, "")))
}

func TestNewPerScraperMetricHubValidatesScraperIDs(t *testing.T) {
	for name, ids := range map[string][]string{
		"none":      nil,
		"empty id":  {"a", ""},
		"duplicate": {"a", "b", "a"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewPerScraperMetricHub(0, 10, ids)
			assert.Error(t, err)
		})
	}
}
