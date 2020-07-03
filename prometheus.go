package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/push"
	"gorm.io/gorm"
)

var (
	_ gorm.Plugin = &Prometheus{}
)

const (
	defaultRefreshInterval = 15   // the prometheus default pull metrics every 15 seconds
	defaultHTTPServerPort  = 8080 // default pull port
)

type MetricsCollector interface {
	Metrics(*Prometheus) []prometheus.Collector
}

type Prometheus struct {
	*gorm.DB
	*DBStats
	*Config
	refreshOnce, pushOnce sync.Once
	Labels                map[string]string
	collectors            []prometheus.Collector
}

type Config struct {
	DBName           string             // use DBName as metrics label
	RefreshInterval  uint32             // refresh metrics interval.
	PushAddr         string             // prometheus pusher address
	StartServer      bool               // if true, create http server to expose metrics
	HTTPServerPort   uint32             // http server port
	MetricsCollector []MetricsCollector // collector
}

func New(config Config) *Prometheus {
	if config.RefreshInterval == 0 {
		config.RefreshInterval = defaultRefreshInterval
	}

	if config.HTTPServerPort == 0 {
		config.HTTPServerPort = defaultHTTPServerPort
	}

	return &Prometheus{Config: &config, Labels: make(map[string]string)}
}

func (p *Prometheus) Name() string {
	return "gorm:prometheus"
}

func (p *Prometheus) Initialize(db *gorm.DB) error { //can be called repeatedly
	p.DB = db

	if p.Config.DBName != "" {
		p.Labels["db_name"] = p.Config.DBName
	}

	p.DBStats = newStats(p.Labels)

	p.refreshOnce.Do(func() {
		for _, mc := range p.MetricsCollector {
			p.collectors = append(p.collectors, mc.Metrics(p)...)
		}

		go func() {
			for range time.Tick(time.Duration(p.Config.RefreshInterval) * time.Second) {
				p.refresh()
			}
		}()
	})

	if p.Config.StartServer {
		go p.startServer()
	}

	if p.PushAddr != "" {
		go p.startPush()
	}

	return nil
}

func (p *Prometheus) refresh() {
	if db, err := p.DB.DB(); err == nil {
		p.DBStats.Set(db.Stats())
	} else {
		p.DB.Logger.Error(context.Background(), "gorm:prometheus failed to collect db status, got error: %v", err)
	}
}

func (p *Prometheus) startPush() {
	p.pushOnce.Do(func() {
		pusher := push.New(p.PushAddr, p.DBName)

		for _, collector := range p.DBStats.Collectors() {
			pusher = pusher.Collector(collector)
		}

		for _, c := range p.collectors {
			pusher = pusher.Collector(c)
		}

		for range time.Tick(time.Duration(p.Config.RefreshInterval) * time.Second) {
			err := pusher.Push()
			if err != nil {
				p.DB.Logger.Error(context.Background(), "gorm:prometheus push err: ", err)
			}
		}
	})
}

var httpServerOnce sync.Once

func (p *Prometheus) startServer() {
	httpServerOnce.Do(func() { //only start once
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(fmt.Sprintf(":%d", p.Config.HTTPServerPort), mux)
		if err != nil {
			p.DB.Logger.Error(context.Background(), "gorm:prometheus listen and serve err: ", err)
		}
	})
}
