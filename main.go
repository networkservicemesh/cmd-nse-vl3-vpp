// Copyright (c) 2022 Cisco and/or its affiliates.
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
	"io/ioutil"
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
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/recvfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/retry"
	"github.com/networkservicemesh/sdk/pkg/networkservice/connectioncontext/ipcontext/vl3"
	registrysendfd "github.com/networkservicemesh/sdk/pkg/registry/common/sendfd"
	"github.com/networkservicemesh/sdk/pkg/tools/opentelemetry"
	"github.com/networkservicemesh/sdk/pkg/tools/token"
	"github.com/networkservicemesh/sdk/pkg/tools/tracing"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/grpcfd"
	"github.com/edwarnicke/vpphelper"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/ipam"
	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/payload"
	registryapi "github.com/networkservicemesh/api/pkg/api/registry"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/tag"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/endpoint"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/chain"
	registryclient "github.com/networkservicemesh/sdk/pkg/registry/chains/client"
	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
)

// Config holds configuration parameters from environment variables
type Config struct {
	Name                  string            `default:"vL3-server" desc:"Name of vL3 Server"`
	DialTimeout           time.Duration     `default:"5s" desc:"timeout to dial NSMgr" split_words:"true"`
	RequestTimeout        time.Duration     `default:"15s" desc:"timeout to request NSE" split_words:"true"`
	ListenOn              string            `default:"listen.on.sock" desc:"listen on socket" split_words:"true"`
	ConnectTo             url.URL           `default:"unix:///var/lib/networkservicemesh/nsm.io.sock" desc:"url to connect to" split_words:"true"`
	MaxTokenLifetime      time.Duration     `default:"10m" desc:"maximum lifetime of tokens" split_words:"true"`
	ServiceNames          []string          `default:"vL3" desc:"Name of providing service" split_words:"true"`
	Labels                map[string]string `default:"" desc:"Endpoint labels"`
	RegisterService       bool              `default:"true" desc:"if true then registers network service on startup" split_words:"true"`
	OpenTelemetryEndpoint string            `default:"otel-collector.observability.svc.cluster.local:4317" desc:"OpenTelemetry Collector Endpoint"`
	PrefixServerURL       url.URL           `default:"vl3-ipam:5006" desc:"URL to VL3 IPAM server"`
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

func startListenPrefixes(ctx context.Context, c *Config, source *workloadapi.X509Source, subscriptions []chan *ipam.PrefixResponse) {
	var previousResponse *ipam.PrefixResponse
	go func() {
		for ctx.Err() == nil {
			cc, err := grpc.DialContext(ctx, grpcutils.URLToTarget(&c.PrefixServerURL), grpc.WithTransportCredentials(
				credentials.NewTLS(
					tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			))
			if err != nil {
				logrus.Error(err.Error())
				time.Sleep(time.Millisecond * 200)
				continue
			}

			managePrefixClient, err := ipam.NewIPAMClient(cc).ManagePrefixes(ctx)
			if err != nil {
				logrus.Error(err.Error())
				time.Sleep(time.Millisecond * 200)
				continue
			}

			request := &ipam.PrefixRequest{
				Type:   ipam.Type_ALLOCATE,
				Prefix: previousResponse.GetPrefix(),
			}

			err = managePrefixClient.Send(request)

			if err != nil {
				time.Sleep(time.Millisecond * 200)
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

func main() {
	// ********************************************************************************
	// setup context to catch signals
	// ********************************************************************************
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	logrus.SetLevel(logrus.TraceLevel)
	logrus.SetFormatter(&nested.Formatter{})
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

	// ********************************************************************************
	// Configure Open Telemetry
	// ********************************************************************************
	if opentelemetry.IsEnabled() {
		collectorAddress := config.OpenTelemetryEndpoint
		spanExporter := opentelemetry.InitSpanExporter(ctx, collectorAddress)
		metricExporter := opentelemetry.InitMetricExporter(ctx, collectorAddress)
		o := opentelemetry.Init(ctx, spanExporter, metricExporter, config.Name)
		defer func() {
			if err := o.Close(); err != nil {
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
					tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			),
		),
	)

	nseRegistryClient := registryclient.NewNetworkServiceEndpointRegistryClient(
		ctx,
		&config.ConnectTo,
		registryclient.WithDialOptions(clientOptions...),
		registryclient.WithNSEAdditionalFunctionality(
			registrysendfd.NewNetworkServiceEndpointRegistryClient(),
		),
	)

	if config.RegisterService {
		for _, serviceName := range config.ServiceNames {
			nsRegistryClient := registryclient.NewNetworkServiceRegistryClient(ctx, &config.ConnectTo, registryclient.WithDialOptions(clientOptions...))
			_, err = nsRegistryClient.Register(ctx, &registryapi.NetworkService{
				Name:    serviceName,
				Payload: payload.IP,
			})
			if err != nil {
				log.FromContext(ctx).Fatalf("unable to register ns %+v", err)
			}
		}
	}

	tmpDir, err := ioutil.TempDir("", config.Name)
	if err != nil {
		logrus.Fatalf("error creating tmpDir %+v", err)
	}
	defer func(tmpDir string) { _ = os.Remove(tmpDir) }(tmpDir)
	listenOn := &(url.URL{Scheme: "unix", Path: filepath.Join(tmpDir, config.ListenOn)})

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 6.2: create and register nse with nsm")
	// ********************************************************************************

	var subscribedChannels []chan *ipam.PrefixResponse
	subscribedChannels = append(subscribedChannels, make(chan *ipam.PrefixResponse, 1))
	var closeAll = func() {
		close(subscribedChannels[0])
	}
	server := createVl3Endpoint(ctx, config, vppConn, source, loopOptions, vrfOptions, subscribedChannels[0])

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

	nseStream, err := nseRegistryClient.Find(ctx, &registryapi.NetworkServiceEndpointQuery{
		NetworkServiceEndpoint: &registryapi.NetworkServiceEndpoint{
			NetworkServiceNames: config.ServiceNames,
		},
	})
	if err != nil {
		log.FromContext(ctx).Fatalf("error getting nses: %+v", err)
	}
	nseList := registryapi.ReadNetworkServiceEndpointList(nseStream)

	for i := 0; i < len(nseList); i++ {
		subscribedChannels = append(subscribedChannels, make(chan *ipam.PrefixResponse, 1))
	}

	startListenPrefixes(ctx, config, source, subscribedChannels)

	for i, nse := range nseList {
		index := i + 1
		if nse.Name == config.Name {
			continue
		}
		if nse.GetInitialRegistrationTime().AsTime().Local().After(nseRegistration.GetInitialRegistrationTime().AsTime().Local()) {
			continue
		}
		vl3Client := createVl3Client(ctx, config, vppConn, source, loopOptions, vrfOptions, subscribedChannels[index])
		log.FromContext(ctx).Infof("connect to %v", nse.String())

		request := &networkservice.NetworkServiceRequest{
			Connection: &networkservice.Connection{
				NetworkServiceEndpointName: nse.Name,
				NetworkService:             config.ServiceNames[0],
				Payload:                    payload.IP,
			},
		}

		requestCtx, cancelRequest := context.WithTimeout(signalCtx, config.RequestTimeout)
		defer cancelRequest()

		conn, err := vl3Client.Request(requestCtx, request)
		if err != nil {
			log.FromContext(ctx).Errorf("request has failed: %v", err.Error())
			continue
		}

		prevClose := closeAll
		closeAll = func() {
			close(subscribedChannels[index])
			closeCtx, cancelClose := context.WithTimeout(ctx, config.RequestTimeout)
			defer cancelClose()
			_, _ = vl3Client.Close(closeCtx, conn)
			prevClose()
		}
	}

	// wait for server to exit
	<-signalCtx.Done()
	closeAll()
	<-vppErrCh
}

func createVl3Client(ctx context.Context, config *Config, vppConn vpphelper.Connection, source *workloadapi.X509Source,
	loopOpts []loopback.Option, vrfOpts []vrf.Option, prefixCh <-chan *ipam.PrefixResponse) networkservice.NetworkServiceClient {
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
					tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			),
		),
	)
	c := client.NewClient(
		ctx,
		client.WithClientURL(&config.ConnectTo),
		client.WithName(config.Name),
		client.WithAdditionalFunctionality(
			vl3.NewClient(ctx, prefixCh),
			up.NewClient(ctx, vppConn, up.WithLoadSwIfIndex(loopback.Load)),
			ipaddress.NewClient(vppConn, ipaddress.WithLoadSwIfIndex(loopback.Load)),
			loopback.NewClient(vppConn, loopOpts...),
			up.NewClient(ctx, vppConn),
			mtu.NewClient(vppConn),
			routes.NewClient(vppConn),
			unnumbered.NewClient(vppConn, loopback.Load),
			vrf.NewClient(vppConn, vrfOpts...),
			memif.NewClient(vppConn),
			sendfd.NewClient(),
			recvfd.NewClient(),
		),
		client.WithDialTimeout(config.DialTimeout),
		client.WithDialOptions(dialOptions...),
	)

	return retry.NewClient(c)
}

func createVl3Endpoint(ctx context.Context, config *Config, vppConn vpphelper.Connection,
	source *workloadapi.X509Source, loopOpts []loopback.Option, vrfOpts []vrf.Option, prefixCh <-chan *ipam.PrefixResponse) *grpc.Server {
	vl3Endpoint := endpoint.NewServer(ctx,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		endpoint.WithName(config.Name),
		endpoint.WithAuthorizeServer(authorize.NewServer()),
		endpoint.WithAdditionalFunctionality(
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
					tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny()),
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
