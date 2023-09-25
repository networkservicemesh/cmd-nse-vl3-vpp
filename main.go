// Copyright (c) 2022-2023 Cisco and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux
// +build linux

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/connectioncontext/ipcontext/ipaddress"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/connectioncontext/ipcontext/routes"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/connectioncontext/ipcontext/unnumbered"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/connectioncontext/mtu"

	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/loopback"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/mechanisms/memif"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/up"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/vrf"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/clientinfo"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/recvfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/null"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/onidle"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/retry"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/upstreamrefresh"
	"github.com/networkservicemesh/sdk/pkg/networkservice/connectioncontext/dnscontext/vl3dns"
	"github.com/networkservicemesh/sdk/pkg/networkservice/connectioncontext/ipcontext/vl3"
	"github.com/networkservicemesh/sdk/pkg/networkservice/connectioncontext/mtu/vl3mtu"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/chain"

	registryclientinfo "github.com/networkservicemesh/sdk/pkg/registry/common/clientinfo"
	registrysendfd "github.com/networkservicemesh/sdk/pkg/registry/common/sendfd"
	"github.com/networkservicemesh/sdk/pkg/tools/opentelemetry"
	"github.com/networkservicemesh/sdk/pkg/tools/token"
	"github.com/networkservicemesh/sdk/pkg/tools/tracing"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/genericsync"
	"github.com/edwarnicke/grpcfd"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/vpphelper"

	"github.com/networkservicemesh/api/pkg/api/ipam"
	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/payload"
	registryapi "github.com/networkservicemesh/api/pkg/api/registry"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/tag"

	kernelsdk "github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/kernel"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/endpoint"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	registryclient "github.com/networkservicemesh/sdk/pkg/registry/chains/client"
	registryauthorize "github.com/networkservicemesh/sdk/pkg/registry/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
)

// Config holds configuration parameters from environment variables
type Config struct {
	Name                   string            `default:"vL3-server" desc:"Name of vL3 Server"`
	DialTimeout            time.Duration     `default:"5s" desc:"timeout to dial NSMgr" split_words:"true"`
	RequestTimeout         time.Duration     `default:"15s" desc:"timeout to request NSE" split_words:"true"`
	ListenOn               string            `default:"listen.on.sock" desc:"listen on socket" split_words:"true"`
	ConnectTo              url.URL           `default:"unix:///var/lib/networkservicemesh/nsm.io.sock" desc:"url to connect to" split_words:"true"`
	MaxTokenLifetime       time.Duration     `default:"10m" desc:"maximum lifetime of tokens" split_words:"true"`
	RegistryClientPolicies []string          `default:"etc/nsm/opa/common/.*.rego,etc/nsm/opa/registry/.*.rego,etc/nsm/opa/client/.*.rego" desc:"paths to files and directories that contain registry client policies" split_words:"true"`
	ServiceNames           []string          `default:"vL3" desc:"Name of providing service" split_words:"true"`
	Labels                 map[string]string `default:"" desc:"Endpoint labels"`
	IdleTimeout            time.Duration     `default:"0" desc:"timeout for automatic shutdown when there were no requests for specified time. Set 0 to disable auto-shutdown." split_words:"true"`
	RegisterService        bool              `default:"true" desc:"if true then registers network service on startup" split_words:"true"`
	OpenTelemetryEndpoint  string            `default:"otel-collector.observability.svc.cluster.local:4317" desc:"OpenTelemetry Collector Endpoint"`
	MetricsExportInterval  time.Duration     `default:"10s" desc:"interval between mertics exports" split_words:"true"`
	PrefixServerURL        url.URL           `default:"vl3-ipam:5006" desc:"URL to VL3 IPAM server" split_words:"true"`
	DNSTemplates           []string          `default:"{{ index .Labels \"podName\" }}.{{ .NetworkService }}." desc:"Represents domain naming templates in go-template format. It is using for generating the domain name for each nse/nsc in the vl3 network" split_words:"true"`
	LogLevel               string            `default:"INFO" desc:"Log level" split_words:"true"`
	dnsServerAddr          net.IP
	dnsServerAddrCh        chan net.IP
	dnsConfigs             genericsync.Map[string, []*networkservice.DNSConfig]
}

// Process prints and processes env to config
func (c *Config) Process() error {
	if err := envconfig.Usage("nsm", c); err != nil {
		return errors.Wrap(err, "cannot show usage of envconfig nse")
	}
	if err := envconfig.Process("nsm", c); err != nil {
		return errors.Wrap(err, "cannot process envconfig nse")
	}
	return nil
}

func startListenPrefixes(ctx context.Context, c *Config, tlsClientConfig *tls.Config, subscriptions []chan *ipam.PrefixResponse) {
	var previousResponse *ipam.PrefixResponse
	go func() {
		var cc *grpc.ClientConn
		var err error
		for ctx.Err() == nil {
			// Close the previous clientConn
			if cc != nil {
				_ = cc.Close()
			}
			dialCtx, dialCtxCancel := context.WithTimeout(ctx, time.Millisecond*200)
			cc, err = grpc.DialContext(dialCtx,
				grpcutils.URLToTarget(&c.PrefixServerURL),
				grpc.WithBlock(),
				grpc.WithTransportCredentials(
					credentials.NewTLS(
						tlsClientConfig,
					),
				),
			)
			// It is safe to cancel dial ctx after DialContext if WithBlock() option is used
			dialCtxCancel()
			if err != nil {
				logrus.Error(err.Error())
				continue
			}

			managePrefixClient, err := ipam.NewIPAMClient(cc).ManagePrefixes(ctx)
			if err != nil {
				logrus.Error(err.Error())
				continue
			}

			request := &ipam.PrefixRequest{
				Type:   ipam.Type_ALLOCATE,
				Prefix: previousResponse.GetPrefix(),
			}

			err = managePrefixClient.Send(request)

			if err != nil {
				continue
			}

			for resp, recvErr := managePrefixClient.Recv(); recvErr == nil; resp, recvErr = managePrefixClient.Recv() {
				if !proto.Equal(previousResponse, resp) {
					previousResponse = resp
					for _, sub := range subscriptions {
						select {
						case sub <- resp:
						default:
						}
					}
				}
			}
		}
	}()
}

const (
	serverSubscriptionIdx = iota
	clientSubscriptionIdx
	totalSubscriptions
)

func main() {
	// ********************************************************************************
	// setup context to catch signals
	// ********************************************************************************
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}

	// ********************************************************************************
	// Configure open tracing
	// ********************************************************************************
	log.EnableTracing(true)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 1: get config from environment")
	// ********************************************************************************
	config := new(Config)
	if err := config.Process(); err != nil {
		logrus.Fatal(err.Error())
	}

	level, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		logrus.Fatalf("invalid log level %s", config.LogLevel)
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&nested.Formatter{})

	config.dnsServerAddrCh = make(chan net.IP, 1)

	// ********************************************************************************
	// Configure Open Telemetry
	// ********************************************************************************
	if opentelemetry.IsEnabled() {
		collectorAddress := config.OpenTelemetryEndpoint
		spanExporter := opentelemetry.InitSpanExporter(ctx, collectorAddress)
		metricExporter := opentelemetry.InitOPTLMetricExporter(ctx, collectorAddress, config.MetricsExportInterval)
		o := opentelemetry.Init(ctx, spanExporter, metricExporter, config.Name)
		defer func() {
			if err = o.Close(); err != nil {
				logrus.Error(err.Error())
			}
		}()
	}

	log.FromContext(ctx).Infof("Config: %#v", config)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 2: run vpp and get a connection to it")
	// ********************************************************************************
	vppConn, vppErrCh := vpphelper.StartAndDialContext(ctx)
	exitOnErr(ctx, cancel, vppErrCh)

	defer func() {
		cancel()
		<-vppErrCh
	}()

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 3: retrieving svid, check spire agent logs if this is the last line you see")
	// ********************************************************************************
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	log.FromContext(ctx).Infof("SVID: %q", svid.ID)

	tlsClientConfig := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny())
	tlsClientConfig.MinVersion = tls.VersionTLS12
	tlsServerConfig := tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny())
	tlsServerConfig.MinVersion = tls.VersionTLS12

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 4: prepare shared between client and server resources")
	// ********************************************************************************

	vrfOptions := []vrf.Option{
		vrf.WithSharedMap(vrf.NewMap()),
		vrf.WithLoadInterface(loopback.Load),
	}
	loopOptions := []loopback.Option{
		loopback.WithSharedMap(loopback.NewMap()),
	}

	signalCtx, cancelSignalCtx := notifyContext(ctx)
	defer cancelSignalCtx()

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 5: get all vl3-nses from registry")
	// ********************************************************************************
	clientOptions := append(tracing.WithTracingDial(),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime))),
		),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsClientConfig,
				),
			),
		),
	)

	clientAdditionalFunctionality := []networkservice.NetworkServiceClient{
		upstreamrefresh.NewClient(ctx, upstreamrefresh.WithLocalNotifications()),
		vl3mtu.NewClient(),
	}
	nsmClient := retry.NewClient(
		client.NewClient(ctx,
			client.WithClientURL(&config.ConnectTo),
			client.WithName(config.Name),
			client.WithAuthorizeClient(authorize.NewClient()),
			client.WithAdditionalFunctionality(
				append(
					clientAdditionalFunctionality,
					clientinfo.NewClient(),
					kernelsdk.NewClient(),
					sendfd.NewClient(),
				)...,
			),
			client.WithDialTimeout(config.DialTimeout),
			client.WithDialOptions(clientOptions...),
		),
	)

	nseRegistryClient := registryclient.NewNetworkServiceEndpointRegistryClient(
		ctx,
		registryclient.WithClientURL(&config.ConnectTo),
		registryclient.WithDialOptions(clientOptions...),
		registryclient.WithNSEAdditionalFunctionality(
			registryclientinfo.NewNetworkServiceEndpointRegistryClient(),
			registrysendfd.NewNetworkServiceEndpointRegistryClient(),
		),
		registryclient.WithAuthorizeNSERegistryClient(registryauthorize.NewNetworkServiceEndpointRegistryClient(
			registryauthorize.WithPolicies(config.RegistryClientPolicies...),
		)),
	)

	if config.RegisterService {
		for _, serviceName := range config.ServiceNames {
			nsRegistryClient := registryclient.NewNetworkServiceRegistryClient(ctx,
				registryclient.WithClientURL(&config.ConnectTo),
				registryclient.WithDialOptions(clientOptions...),
				registryclient.WithAuthorizeNSRegistryClient(registryauthorize.NewNetworkServiceRegistryClient(
					registryauthorize.WithPolicies(config.RegistryClientPolicies...),
				)))
			_, err = nsRegistryClient.Register(ctx, &registryapi.NetworkService{
				Name:    serviceName,
				Payload: payload.IP,
			})
			if err != nil {
				log.FromContext(ctx).Fatalf("unable to register ns %+v", err)
			}
		}
	}

	tmpDir, err := os.MkdirTemp("", config.Name)
	if err != nil {
		logrus.Fatalf("error creating tmpDir %+v", err)
	}
	defer func(tmpDir string) { _ = os.Remove(tmpDir) }(tmpDir)
	listenOn := &(url.URL{Scheme: "unix", Path: filepath.Join(tmpDir, config.ListenOn)})

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 6.2: create and register nse with nsm")
	// ********************************************************************************

	var subscribedChannels []chan *ipam.PrefixResponse
	for i := 0; i < totalSubscriptions; i++ {
		subscribedChannels = append(subscribedChannels, make(chan *ipam.PrefixResponse, 1))
	}
	var closeSubscribedChannels = func() {
		for i := 0; i < totalSubscriptions; i++ {
			close(subscribedChannels[i])
		}
	}
	startListenPrefixes(ctx, config, tlsClientConfig, subscribedChannels)

	server := createVl3Endpoint(ctx, cancel, config, vppConn, tlsServerConfig, source, loopOptions, vrfOptions, subscribedChannels[serverSubscriptionIdx])

	srvErrCh := grpcutils.ListenAndServe(ctx, listenOn, server)
	exitOnErr(ctx, cancel, srvErrCh)
	log.FromContext(ctx).Infof("grpc server started")

	nseRegistration := &registryapi.NetworkServiceEndpoint{
		Name:                 config.Name,
		NetworkServiceNames:  config.ServiceNames,
		NetworkServiceLabels: make(map[string]*registryapi.NetworkServiceLabels),
		Url:                  listenOn.String(),
	}
	for _, serviceName := range config.ServiceNames {
		nseRegistration.NetworkServiceLabels[serviceName] = &registryapi.NetworkServiceLabels{Labels: config.Labels}
	}
	nseRegistration, err = nseRegistryClient.Register(ctx, nseRegistration)
	log.FromContext(ctx).Infof("registered nse: %+v", nseRegistration)

	if err != nil {
		log.FromContext(ctx).Fatalf("unable to register nse %+v", err)
	}

	// Update the nseList to make sure we have all registered vl3-endpoints
	nseStream, err := nseRegistryClient.Find(ctx, &registryapi.NetworkServiceEndpointQuery{
		NetworkServiceEndpoint: &registryapi.NetworkServiceEndpoint{
			NetworkServiceNames: config.ServiceNames,
		},
	})

	if err != nil {
		log.FromContext(ctx).Fatalf("error getting nses: %+v", err)
	}
	var nseList = registryapi.ReadNetworkServiceEndpointList(nseStream)

	requestCtx, cancelRequest := context.WithTimeout(signalCtx, config.RequestTimeout)
	defer cancelRequest()

	conn, err := nsmClient.Request(requestCtx, &networkservice.NetworkServiceRequest{
		Connection: &networkservice.Connection{
			Id:                         config.Name + "-kernel",
			NetworkServiceEndpointName: config.Name,
			NetworkService:             config.ServiceNames[0],
			Payload:                    payload.IP,
		},
	})

	if err != nil {
		log.FromContext(ctx).Fatal(err.Error())
	}

	defer func(conn *networkservice.Connection) {
		closeCtx, cancelClose := context.WithTimeout(ctx, config.RequestTimeout)
		defer cancelClose()
		_, _ = nsmClient.Close(closeCtx, conn)
	}(conn)

	config.dnsServerAddr = conn.GetContext().GetIpContext().GetSrcIPNets()[0].IP
	config.dnsServerAddrCh <- conn.GetContext().GetIpContext().GetSrcIPNets()[0].IP

	vl3Client := createVl3Client(ctx, config, vppConn, tlsClientConfig, source, loopOptions, vrfOptions, subscribedChannels[clientSubscriptionIdx], clientAdditionalFunctionality...)
	for _, nse := range nseList {
		if nse.Name == config.Name {
			continue
		}
		if nse.GetInitialRegistrationTime().AsTime().Local().After(nseRegistration.GetInitialRegistrationTime().AsTime().Local()) {
			continue
		}

		log.FromContext(ctx).Infof("connect to %v", nse.String())

		request := &networkservice.NetworkServiceRequest{
			Connection: &networkservice.Connection{
				NetworkServiceEndpointName: nse.Name,
				NetworkService:             config.ServiceNames[0],
				Payload:                    payload.IP,
			},
		}

		requestCtx, cancelRequest = context.WithTimeout(signalCtx, config.RequestTimeout)
		defer cancelRequest()

		conn, err := vl3Client.Request(requestCtx, request)
		if err != nil {
			log.FromContext(ctx).Errorf("request has failed: %v", err.Error())
			continue
		}

		defer func() {
			closeCtx, cancelClose := context.WithTimeout(ctx, config.RequestTimeout)
			defer cancelClose()
			_, _ = vl3Client.Close(closeCtx, conn)
		}()
	}

	// wait for server to exit
	<-signalCtx.Done()
	closeSubscribedChannels()
}

func createVl3Client(ctx context.Context, config *Config, vppConn vpphelper.Connection, tlsClientConfig *tls.Config, source x509svid.Source,
	loopOpts []loopback.Option, vrfOpts []vrf.Option, prefixCh <-chan *ipam.PrefixResponse, clientAdditionalFunctionality ...networkservice.NetworkServiceClient) networkservice.NetworkServiceClient {
	dialOptions := append(tracing.WithTracingDial(),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime))),
		),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsClientConfig,
				),
			),
		),
	)
	c := client.NewClient(
		ctx,
		client.WithClientURL(&config.ConnectTo),
		client.WithName(config.Name),
		client.WithAdditionalFunctionality(
			append(
				clientAdditionalFunctionality,
				vl3.NewClient(ctx, prefixCh),
				vl3dns.NewClient(config.dnsServerAddr, &config.dnsConfigs),
				up.NewClient(ctx, vppConn, up.WithLoadSwIfIndex(loopback.Load)),
				ipaddress.NewClient(vppConn, ipaddress.WithLoadSwIfIndex(loopback.Load)),
				loopback.NewClient(vppConn, loopOpts...),
				up.NewClient(ctx, vppConn),
				mtu.NewClient(vppConn),
				routes.NewClient(vppConn),
				unnumbered.NewClient(vppConn, loopback.Load),
				vrf.NewClient(vppConn, vrfOpts...),
				memif.NewClient(ctx, vppConn),
				sendfd.NewClient(),
				recvfd.NewClient(),
			)...,
		),
		client.WithHealClient(null.NewClient()),
		client.WithDialTimeout(config.DialTimeout),
		client.WithDialOptions(dialOptions...),
	)

	return retry.NewClient(c)
}

func createVl3Endpoint(ctx context.Context, cancel context.CancelFunc, config *Config, vppConn vpphelper.Connection, tlsServerConfig *tls.Config,
	source x509svid.Source, loopOpts []loopback.Option, vrfOpts []vrf.Option, prefixCh <-chan *ipam.PrefixResponse) *grpc.Server {
	vl3Endpoint := endpoint.NewServer(ctx,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		endpoint.WithName(config.Name),
		endpoint.WithAuthorizeServer(authorize.NewServer()),
		endpoint.WithAdditionalFunctionality(
			onidle.NewServer(ctx, cancel, config.IdleTimeout),
			vl3dns.NewServer(ctx,
				config.dnsServerAddrCh,
				vl3dns.WithDomainSchemes(config.DNSTemplates...),
				vl3dns.WithConfigs(&config.dnsConfigs),
			),
			vl3mtu.NewServer(),
			vl3.NewServer(ctx, prefixCh),
			up.NewServer(ctx, vppConn, up.WithLoadSwIfIndex(loopback.Load)),
			ipaddress.NewServer(vppConn, ipaddress.WithLoadSwIfIndex(loopback.Load)),
			unnumbered.NewServer(vppConn, loopback.Load),
			vrf.NewServer(vppConn, vrfOpts...),
			loopback.NewServer(vppConn, loopOpts...),
			mechanisms.NewServer(map[string]networkservice.NetworkServiceServer{
				memif.MECHANISM: chain.NewNetworkServiceServer(
					sendfd.NewServer(),
					up.NewServer(ctx, vppConn),
					mtu.NewServer(vppConn),
					routes.NewServer(vppConn),
					tag.NewServer(ctx, vppConn),
					memif.NewServer(ctx, vppConn),
				),
			}),
		),
	)

	options := append(
		tracing.WithTracing(),
		grpc.Creds(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsServerConfig,
				),
			),
		),
	)
	server := grpc.NewServer(options...)
	vl3Endpoint.Register(server)

	return server
}

func exitOnErr(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}

func notifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(
		ctx,
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
}
