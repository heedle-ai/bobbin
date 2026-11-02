module github.com/heedle-ai/bobbin

go 1.24.0

require (
	github.com/heedle-ai/draft v0.0.0
	github.com/heedle-ai/shuttle v0.0.0
	github.com/heedle-ai/weft v0.0.0
	github.com/openshift/imagebuilder v1.2.6-0.20231110114814-35a50d57f722
	go.temporal.io/sdk v1.41.1
)

require (
	github.com/alecthomas/participle/v2 v2.1.1 // indirect
	github.com/containerd/typeurl v1.0.2 // indirect
	github.com/containers/storage v1.46.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/facebookgo/clock v0.0.0-20150410010913-600d898af40a // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0 // indirect
	github.com/moby/buildkit v0.10.6 // indirect
	github.com/moby/sys/mountinfo v0.6.2 // indirect
	github.com/nexus-rpc/sdk-go v0.6.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/robfig/cron v1.2.0 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/stretchr/testify v1.10.0 // indirect
	go.temporal.io/api v1.62.2 // indirect
	golang.org/x/net v0.39.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	golang.org/x/time v0.3.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/grpc v1.67.1 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Until the heedle-ai modules are published, build against sibling checkouts.
replace (
	github.com/heedle-ai/draft => ../draft
	github.com/heedle-ai/shuttle => ../shuttle
	github.com/heedle-ai/weft => ../weft
)
