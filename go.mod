module github.com/networkservicemesh/cmd-nse-vl3-vpp

go 1.16

require (
	git.fd.io/govpp.git v0.3.6-0.20210927044411-385ccc0d8ba9
	github.com/antonfisher/nested-logrus-formatter v1.3.1
	github.com/edwarnicke/govpp v0.0.0-20220311182453-f32f292e0e91
	github.com/edwarnicke/grpcfd v1.1.2
	github.com/edwarnicke/serialize v1.0.7
	github.com/edwarnicke/vpphelper v0.0.0-20210225052320-b4f1f1aff45d
	github.com/golang/protobuf v1.5.2
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/networkservicemesh/api v1.2.1-0.20220315001249-f33f8c3f2feb
	github.com/networkservicemesh/sdk v0.5.1-0.20220316105041-b88289b9022e
	github.com/networkservicemesh/sdk-vpp v0.0.0-20220316102406-992ae319ddbe
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.8.1
	github.com/spiffe/go-spiffe/v2 v2.0.0-beta.8
	google.golang.org/grpc v1.42.0
	google.golang.org/protobuf v1.27.1
)

replace github.com/networkservicemesh/sdk => ../sdk

replace github.com/networkservicemesh/sdk-vpp => ../sdk-vpp
