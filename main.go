package main

import (
	"flag"
)

var AppGitState = "unknown"
var AppGitCommit = "unknown"
var AppVersion = "unknown"

var listenAddress = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
var serviceName = flag.String("service-name", "kube-latency", "The name of the clusterIP less kubernetes service.")
var dataSize = flag.Int("data-size", 16*1024*1024, "The size in bytes of the data call.")
var testFrequency = flag.Int("test-frequency", 10, "How often test are performed.")

func main() {
	a := NewApp()
	a.Run()
}
