/*
 * Copyright (c) Facebook, Inc. and its affiliates.
 *
 * This source code is licensed under the MIT license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"google.golang.org/grpc"

	hubgrpc "github.com/blockcast/prometheus-edge-hub/grpc"
	"github.com/blockcast/prometheus-edge-hub/hub"
	"github.com/labstack/echo/v4"
)

const (
	defaultPort                = 9091
	defaultGRPCPort            = 0
	defaultLimit               = -1
	defaultScrapeTimeout       = 10                 // seconds
	defaultMaxGRPCMsgSizeBytes = 1024 * 1024 * 1024 //1 GB
)

func main() {
	port := flag.Int("port", defaultPort, fmt.Sprintf("Port to listen for requests. Default is %d", defaultPort))
	totalMetricsLimit := flag.Int("limit", defaultLimit, fmt.Sprintf("Limit the total metrics in the hub at one time. Will reject a push if hub is full. Default is %d which is no limit.", defaultLimit))
	scrapeTimeout := flag.Int("scrapeTimeout", defaultScrapeTimeout, fmt.Sprintf("Timeout for scrape calls. Default is %d", defaultScrapeTimeout))
	grpcPort := flag.Int("grpc-port", defaultGRPCPort, fmt.Sprintf("Port to listen for GRPC requests"))
	grpcMaxGRPCMsgSizeBytes := flag.Int("grpc-max-msg-size", defaultMaxGRPCMsgSizeBytes, fmt.Sprintf("Max message size (bytes) for GRPC receives"))
	scrapers := flag.String("scrapers", "", fmt.Sprintf("Comma-separated scraper ids. When set, every push is kept once per scraper and GET /metrics?%s=<id> drains only that scraper's copy, so replicated Prometheus servers scraping one hub each receive every datapoint; -limit then bounds each scraper's buffer. When empty, the hub keeps one buffer that the first scrape drains.", hub.ScraperQueryParam))
	flag.Parse()

	metricHub := hub.NewMetricHub(*totalMetricsLimit, *scrapeTimeout)
	if *scrapers != "" {
		if *totalMetricsLimit <= 0 {
			log.Fatal("-limit must be positive when -scrapers is set; each scraper buffer needs a finite bound")
		}
		perScraper, err := hub.NewPerScraperMetricHub(*totalMetricsLimit, *scrapeTimeout, strings.Split(*scrapers, ","))
		if err != nil {
			log.Fatalf("invalid -scrapers %q: %v", *scrapers, err)
		}
		metricHub = perScraper
	}
	e := echo.New()

	e.POST("/metrics", metricHub.Receive)
	e.GET("/metrics", metricHub.Scrape)

	e.GET("/debug", metricHub.Debug)

	// For liveness probe
	e.GET("/", func(ctx echo.Context) error { return ctx.NoContent(http.StatusOK) })

	e.GET("/internal", serveInternalMetrics)

	if *grpcPort != 0 {
		go func() {
			log.Fatal(serveGRPC(*grpcPort, *grpcMaxGRPCMsgSizeBytes, metricHub))
		}()
	}

	go e.Logger.Fatal(e.Start(fmt.Sprintf(":%d", *port)))
}

func serveInternalMetrics(ctx echo.Context) error {
	text, err := hub.WriteInternalMetrics()
	if err != nil {
		return ctx.NoContent(http.StatusInternalServerError)
	}
	return ctx.String(http.StatusOK, text)
}

func serveGRPC(port, maxMsgSize int, metricHub *hub.MetricHub) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	metricsGrpcServer := hubgrpc.MetricsControllerServerImpl{MetricHub: metricHub}
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(maxMsgSize))
	hubgrpc.RegisterMetricsControllerServer(grpcServer, &metricsGrpcServer)

	log.Printf("Serving GRPC on: %d\n", port)

	return grpcServer.Serve(lis)
}
