package php

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/MeABc/glog"
	"github.com/MeABc/net/http2"
	"github.com/cloudflare/golibs/lrucache"
	"golang.org/x/sync/singleflight"

	"../../filters"
	"../../helpers"
	"../../proxy"
	"../../storage"
)

const (
	filterName string = "php"
)

type Config struct {
	Servers []struct {
		URL       string
		Password  string
		SSLVerify bool
		Host      string
	}
	Transport struct {
		Dialer struct {
			Timeout        int
			KeepAlive      int
			DualStack      bool
			DNSCacheExpiry int
			DNSCacheSize   uint
		}
		Proxy struct {
			Enabled bool
			URL     string
		}
		EnableRemoteDNS     bool
		DNSServer           string
		DisableKeepAlives   bool
		DisableCompression  bool
		TLSHandshakeTimeout int
		MaxIdleConnsPerHost int
	}
}

type Filter struct {
	Config
	Transport *Transport
}

func init() {
	filters.Register(filterName, func() (filters.Filter, error) {
		filename := filterName + ".json"
		config := new(Config)
		err := storage.LookupStoreByFilterName(filterName).UnmarshallJson(filename, config)
		if err != nil {
			glog.Fatalf("storage.ReadJsonConfig(%#v) failed: %s", filename, err)
		}
		return NewFilter(config)
	})
}

func NewFilter(config *Config) (filters.Filter, error) {
	servers := make([]Server, 0)
	for _, s := range config.Servers {
		u, err := url.Parse(s.URL)
		if err != nil {
			return nil, err
		}

		server := Server{
			URL:       u,
			Password:  s.Password,
			SSLVerify: s.SSLVerify,
			Host:      s.Host,
		}

		servers = append(servers, server)
	}

	d0 := &net.Dialer{
		KeepAlive: time.Duration(config.Transport.Dialer.KeepAlive) * time.Second,
		Timeout:   time.Duration(config.Transport.Dialer.Timeout) * time.Second,
		DualStack: config.Transport.Dialer.DualStack,
	}

	d := &helpers.Dialer{
		Dialer: d0,
		Resolver: &helpers.Resolver{
			Singleflight: &singleflight.Group{},
			LRUCache:     lrucache.NewLRUCache(config.Transport.Dialer.DNSCacheSize),
			Hosts:        lrucache.NewLRUCache(32),
			DNSExpiry:    time.Duration(config.Transport.Dialer.DNSCacheExpiry) * time.Second,
		},
		Level: 2,
	}

	if config.Transport.EnableRemoteDNS {
		d.Resolver.DNSServer = config.Transport.DNSServer
		_, _, _, err := helpers.ParseIPPort(config.Transport.DNSServer)
		if err != nil {
			glog.Fatalf("helpers.ParseIPPort(%v) failed", config.Transport.DNSServer)
		}
	}

	for _, server := range servers {
		if server.Host != "" {
			d.Resolver.Hosts.Set(server.URL.Hostname(), server.Host, time.Time{})
		}
	}

	tr := &http.Transport{
		Dial: d.Dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
			ClientSessionCache: tls.NewLRUClientSessionCache(1000),
		},
		TLSHandshakeTimeout: time.Duration(config.Transport.TLSHandshakeTimeout) * time.Second,
		MaxIdleConnsPerHost: config.Transport.MaxIdleConnsPerHost,
	}

	if config.Transport.Proxy.Enabled {
		fixedURL, err := url.Parse(config.Transport.Proxy.URL)
		if err != nil {
			glog.Fatalf("url.Parse(%#v) error: %s", config.Transport.Proxy.URL, err)
		}

		dialer, err := proxy.FromURL(fixedURL, d, nil)
		if err != nil {
			glog.Fatalf("proxy.FromURL(%#v) error: %s", fixedURL.String(), err)
		}

		tr.Dial = dialer.Dial
		tr.DialTLS = nil
		tr.Proxy = nil
	}

	if tr.TLSClientConfig != nil {
		err := http2.ConfigureTransport(tr)
		if err != nil {
			glog.Warningf("PHP: Error enabling Transport HTTP/2 support: %v", err)
		}
	}

	return &Filter{
		Config: *config,
		Transport: &Transport{
			RoundTripper: tr,
			Servers:      servers,
		},
	}, nil
}

func (p *Filter) FilterName() string {
	return filterName
}

func (f *Filter) RoundTrip(ctx context.Context, req *http.Request) (context.Context, *http.Response, error) {
	resp, err := f.Transport.RoundTrip(req)
	if err != nil {
		helpers.CloseResponseBody(resp)
		return ctx, nil, err
	}
	glog.V(2).Infof("%s \"PHP %s %s %s\" %d %s", req.RemoteAddr, req.Method, req.URL.String(), req.Proto, resp.StatusCode, resp.Header.Get("Content-Length"))

	return ctx, resp, nil
}
