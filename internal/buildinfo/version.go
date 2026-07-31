package buildinfo

// Version is the string reported by GET / and every /v1 response.
//
// Release builds override it at link time (see build_all.sh):
//
//	go build -ldflags "-X github.com/trinity-aml/flaresolverr-go/internal/buildinfo.Version=v1.0.7"
//
// The literal below is the fallback for `go run` and plain `go build`, so it
// only needs bumping when the last released version should be the default for
// unstamped builds.
var Version = "1.0.6"
