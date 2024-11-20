package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	slogmulti "github.com/samber/slog-multi"
	slogsampling "github.com/samber/slog-sampling"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	logger    *slog.Logger
	clientset *kubernetes.Clientset

	namespace, app, nodesFile, kubeConfig string
	peerPort, apiPort, healthPort         int
	debug                                 bool

	readyz   bool
	readyzmu sync.Mutex
)

func init() {

	flag.StringVar(&kubeConfig, "kubeconfig", "config", "kubeconfig file in ~/.kube to work with")
	flag.StringVar(&namespace, "namespace", "typesense", "namespace that typesense is installed within")
	flag.StringVar(&app, "app", "typesense-sts", "name of the typesense app to watch the pods of")
	flag.StringVar(&nodesFile, "nodes-file", "/usr/share/typesense/nodes", "location of the file to write node information to")
	flag.IntVar(&peerPort, "peer-port", 8107, "port on which typesense peering service listens")
	flag.IntVar(&apiPort, "api-port", 8108, "port on which typesense API service listens")
	flag.BoolVar(&debug, "debug", false, "debug mode")
	flag.IntVar(&healthPort, "health-port", 8080, "port on which peer-resolver health endpoints listen")
	flag.Parse()

	levelInfo := slog.LevelInfo
	if debug {
		levelInfo = slog.LevelDebug
	}

	option := slogsampling.ThresholdSamplingOption{
		Tick:      3 * time.Second,
		Threshold: 10,
		Rate:      0.1,

		Matcher: slogsampling.MatchAll(),
	}

	logger = slog.New(
		slogmulti.
			Pipe(option.NewMiddleware()).
			Handler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
				Level: levelInfo,
			})),
	)

	slog.SetDefault(logger)

	logger.Info("starting peer-resolver")
}

func main() {
	defer func() {
		logger.Info("shutting down peer-resolver")
	}()

	configPath := filepath.Join(homedir.HomeDir(), ".kube", kubeConfig)
	var config *rest.Config
	var err error

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		// No config file found, fall back to in-cluster config.
		config, err = rest.InClusterConfig()
		if err != nil {
			logger.Error(fmt.Sprintf("failed to build local config: %s", err))
			os.Exit(1)
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			logger.Error(fmt.Sprintf("failed to build in-cluster config: %s", err))
			os.Exit(1)
		}
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create kubernetes client: %s", err))
		os.Exit(1)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go watch(&wg, nodesFile, namespace, app, peerPort, apiPort)

	wg.Add(1)
	go serve(&wg, healthPort)

	wg.Wait()
}

func serve(wg *sync.WaitGroup, healthPort int) {
	defer wg.Done()

	logger.Info("listening to health endpoints", "port", healthPort)

	http.HandleFunc("/livez", livezHandler)
	http.HandleFunc("/readyz/", readyzHandler)

	addr := fmt.Sprintf(":%d", healthPort)
	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 3 * time.Second,
	}

	err := server.ListenAndServe()
	if err != nil {
		logger.Error(fmt.Sprintf("failed to serve a mux: %s", err))
		os.Exit(1)
	}
}

func livezHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func readyzHandler(w http.ResponseWriter, r *http.Request) {
	defer readyzmu.Unlock()

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}

	readyzmu.Lock()
	if !readyz {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func watch(wg *sync.WaitGroup, nodesFile, namespace, app string, peerPort, apiPort int) {
	defer func() {
		readyzmu.Lock()
		readyz = false
		readyzmu.Unlock()

		logger.Info("stop watching for pods", "namespace", namespace, "app", app, "peerPort", peerPort, "apiPort", apiPort)
		wg.Done()
	}()

	logger.Info("creating pods watcher")
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(context.Background(), metav1.ListOptions{})
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create pods watcher: %s", err))
		os.Exit(1)
	}

	readyzmu.Lock()
	readyz = true
	readyzmu.Unlock()

	logger.Info("watching for pods", "namespace", namespace, "app", app, "peerPort", peerPort, "apiPort", apiPort)

	for range watcher.ResultChan() {
		nodes := getNodes(namespace, app, peerPort, apiPort)
		if strings.TrimSpace(nodes) != "" {
			err := os.WriteFile(nodesFile, []byte(nodes), 0600)
			if err != nil {
				logger.Error(fmt.Sprintf("failed to write nodes file: %s", err))
			}
		}
	}
}

func getNodes(namespace, app string, peerPort, apiPort int) string {
	var nodes []string

	listOptions := metav1.ListOptions{
		LabelSelector: "app=" + app,
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), listOptions)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to list pods: %s", err))
		return ""
	}

	if len(pods.Items) == 0 {
		logger.Info(fmt.Sprintf("no pods found for app: %s/%s", namespace, app))
		return ""
	}

	for _, pod := range pods.Items {
		for containerIdx, container := range pod.Spec.Containers {
			if container.Name != "typesense" {
				continue
			}

			for _, port := range container.Ports {
				if int(port.ContainerPort) == apiPort {
					ready := false
					if containerIdx <= len(pod.Status.ContainerStatuses)-1 {
						ready = pod.Status.ContainerStatuses[containerIdx].Ready
					}

					if strings.TrimSpace(pod.Status.PodIP) != "" {
						if !ready && pod.CreationTimestamp.Add(3*time.Minute).Before(time.Now().UTC()) {
							err = deleteNode(namespace, pod.Name)
							if err != nil {
								logger.Error(fmt.Sprintf("failed to delete pod: %s", err), "creationTimestamp", pod.CreationTimestamp)
							}

							continue
						}

						logger.Debug("adding pod to group configuration", "namespace", namespace, "pod", pod.Name, "ready", ready)
						nodes = append(nodes, fmt.Sprintf("%s:%d:%d", pod.Status.PodIP, peerPort, port.ContainerPort))
					}
				}
			}
		}
	}

	typesenseNodes := strings.Join(nodes, ",")

	if len(nodes) != 0 {
		logger.Info("new group configuration", "nodes", len(nodes), "group", nodes)
	} else {
		logger.Warn("empty group configuration", "nodes", len(nodes), "group", nodes)
	}

	return typesenseNodes
}

func deleteNode(namespace, podName string) error {
	logger.Info(fmt.Sprintf("deleting pod %s: not ready for more than 3min", podName))
	return clientset.CoreV1().Pods(namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
}
