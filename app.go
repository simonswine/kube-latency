package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/dustin/randbo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tcnksm/go-httpstat"
	kubemeta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	kubeapi_v1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

const EnvKubeNamespace = "KUBE_NAMESPACE"
const EnvKubePodName = "KUBE_POD_NAME"

type App struct {
	rand io.Reader

	kubeClient    *kubernetes.Clientset
	kubeNamespace string
	kubePodName   string
	myService     *kubeapi_v1.Service
	zonePerNode   map[string]string

	metricDownloadProbeSize *prometheus.GaugeVec
	metricDownloadDurations *prometheus.SummaryVec
	metricPingDurations     *prometheus.SummaryVec
}

type Labels struct {
	PodName  string
	PodIP    net.IP
	NodeName string
	Zone     string
}

func LabelsKeys(prefix string) []string {
	return []string{
		prefix + "pod_name",
		prefix + "pod_ip",
		prefix + "zone",
		prefix + "node_name",
	}
}
func (l *Labels) Values() []string {
	return []string{
		l.PodName,
		l.PodIP.String(),
		l.Zone,
		l.NodeName,
	}
}

func NewApp() *App {
	var labels []string
	labels = append(labels, LabelsKeys("source_")...)
	labels = append(labels, LabelsKeys("dest_")...)
	return &App{
		rand: randbo.New(),
		metricDownloadProbeSize: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "download_probe_size",
				Help: "Download probe sizes in bytes",
			},
			labels,
		),
		metricDownloadDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "download_durations_s",
				Help:       "Download durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		metricPingDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "ping_durations_s",
				Help:       "Ping durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		zonePerNode: map[string]string{},
	}
}

func (a *App) handlePing(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "pong")
}

func (a *App) handleData(w http.ResponseWriter, r *http.Request) {
	_, err := io.CopyN(w, a.rand, int64(*dataSize))
	if err != nil {
		log.Warn("failed to write random data: ", err)
	}
}

// get zone of node and cache in hashmap, don't cache not found nodes
func (a *App) getZoneForNode(nodeName string) string {
	if zone, ok := a.zonePerNode[nodeName]; ok {
		return zone
	}

	node, err := a.kubeClient.Nodes().Get(nodeName, kubemeta_v1.GetOptions{})
	if err != nil {
		log.Warnf("error getting node %s: %s", nodeName, err)
		return ""
	}

	zone, _ := node.Labels[kubemeta_v1.LabelZoneFailureDomain]
	a.zonePerNode[nodeName] = zone

	return zone
}

func (a *App) testLoop() {
	for {
		// get latest pod list
		var serviceLabels labels.Set = a.myService.Spec.Selector
		podsList, err := a.kubeClient.Pods(a.kubeNamespace).List(kubemeta_v1.ListOptions{
			LabelSelector: serviceLabels.AsSelector().String(),
		})
		if err != nil {
			log.Warn("failed to list pods with selector '%s': %s", serviceLabels.AsSelector().String(), err)
		}

		destinations := []*Labels{}
		var source *Labels = nil

		// build labels per pod
		for _, pod := range podsList.Items {
			podIP := net.ParseIP(pod.Status.PodIP)
			if podIP == nil {
				continue
			}
			l := &Labels{
				Zone:     a.getZoneForNode(pod.Spec.NodeName),
				PodName:  pod.Name,
				PodIP:    podIP,
				NodeName: pod.Spec.NodeName,
			}
			if pod.Name == a.kubePodName {
				source = l
			} else {
				destinations = append(destinations, l)
			}
		}

		// run tests if pods found
		if source == nil || len(destinations) == 0 {
			log.Info("skip tests no suitable pods found")
		} else {
			for _, dest := range destinations {
				go a.testPing(source, dest)
				go a.testDownload(source, dest)
			}
		}

		time.Sleep(time.Duration(*testFrequency) * time.Second)
	}
}

func (a *App) getPodLabels(podName string) (*Labels, error) {
	// get my pod object
	pod, err := a.kubeClient.Pods(a.kubeNamespace).Get(podName, kubemeta_v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to get pod %s/%s: %s", a.kubeNamespace, a.kubePodName, err)
	}

	// get my node object
	node, err := a.kubeClient.Nodes().Get(pod.Spec.NodeName, kubemeta_v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to get node %s: %s", pod.Spec.NodeName, err)
	}

	ip := net.ParseIP(pod.Status.PodIP)
	if ip == nil {
		return nil, fmt.Errorf("error parsing podIP %s: %s", pod.Status.PodIP)
	}

	return &Labels{
		PodIP:    ip,
		PodName:  podName,
		NodeName: node.Name,
	}, nil
}

func (a *App) Run() {
	log.Infof("starting kube-latency v%s (git %s, %s)", AppVersion, AppGitCommit, AppGitState)

	// parse cli flags
	flag.Parse()

	// get environment variables
	a.kubeNamespace = os.Getenv(EnvKubeNamespace)
	if a.kubeNamespace == "" {
		log.Fatalf("please specify %s", EnvKubeNamespace)
	}
	a.kubePodName = os.Getenv(EnvKubePodName)
	if a.kubePodName == "" {
		log.Fatalf("please specify %s", EnvKubePodName)
	}

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	a.kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// get my service
	a.myService, err = a.kubeClient.Services(a.kubeNamespace).Get(*serviceName, kubemeta_v1.GetOptions{})
	if err != nil {
		log.Fatalf("failed to get my service %s/%s: %s", a.kubeNamespace, *serviceName, err)
	}

	// register prometheus metrics
	prometheus.MustRegister(a.metricDownloadProbeSize)
	prometheus.MustRegister(a.metricDownloadDurations)
	prometheus.MustRegister(a.metricPingDurations)

	// start periodic test
	go a.testLoop()

	// start webserver
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/ping", a.handlePing)
	http.HandleFunc("/data", a.handleData)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

func (a *App) testDownload(source, dest *Labels) {
	url := fmt.Sprintf("http://%s:8080/data", dest.PodIP.String())
	result, end, err := a.testHTTP(url)
	if err != nil {
		log.Warnf("test download from '%s' failed: %s", url, err)
	}
	var labels []string
	labels = append(labels, source.Values()...)
	labels = append(labels, dest.Values()...)

	a.metricDownloadDurations.WithLabelValues(labels...).Observe(result.ContentTransfer(end).Seconds())
	a.metricDownloadProbeSize.WithLabelValues(labels...).Set(float64(*dataSize))
}

func (a *App) testPing(source, dest *Labels) {
	url := fmt.Sprintf("http://%s:8080/ping", dest.PodIP.String())
	var labels []string
	labels = append(labels, source.Values()...)
	labels = append(labels, dest.Values()...)
	for i := 1; i <= 10; i++ {
		result, end, err := a.testHTTP(url)
		if err != nil {
			log.Warnf("test ping from '%s' failed: %s", url, err)
		}
		a.metricPingDurations.WithLabelValues(labels...).Observe(result.ContentTransfer(end).Seconds())
	}
}

func (a *App) testHTTP(url string) (httpstat.Result, time.Time, error) {

	// Create a new HTTP request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Create a httpstat powered context
	var result httpstat.Result
	ctx := httpstat.WithHTTPStat(req.Context(), &result)
	req = req.WithContext(ctx)
	// Send request by default HTTP client
	client := http.DefaultClient
	res, err := client.Do(req)
	if err != nil {
		return result, time.Time{}, err
	}
	if _, err := io.Copy(ioutil.Discard, res.Body); err != nil {
		return result, time.Time{}, err
	}
	res.Body.Close()
	end := time.Now()
	return result, end, nil
}
