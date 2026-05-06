module github.com/sstriker/cmake-to-bazel

go 1.25.0

require (
	github.com/bazelbuild/remote-apis v0.0.0-20260331222004-becdd8f9ff81
	github.com/google/uuid v1.6.0
	google.golang.org/genproto v0.0.0-20250603155806-513f23925822
	google.golang.org/genproto/googleapis/bytestream v0.0.0-20260427160629-7cedc36a6bc4
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260203192932-546029d2fa20
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cloud.google.com/go/longrunning v0.8.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260203192932-546029d2fa20 // indirect
)
