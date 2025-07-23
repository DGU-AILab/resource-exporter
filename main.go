package main
import (
	"context"
	"os"
	"strings"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
	"net"
	"errors"
	"github.com/shirou/gopsutil/v3/process"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
    	"k8s.io/client-go/tools/clientcmd"
    	"path/filepath"
)
var (
	// define prometheus metrics
	gpuTemp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpu_temperature_celsius",
			Help: "GPU temperature in Celsius",
		},
		[]string{"gpu_uuid", "gpu_name", "hostname"}, 
	)

	gpuMemTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpu_memory_total_bytes",
			Help: "Total GPU memory in bytes",
		},
		[]string{"gpu_uuid", "gpu_name", "hostname"},
	)

	gpuMemUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpu_memory_used_bytes",
			Help: "Used GPU memory in bytes",
		},
		[]string{"gpu_uuid", "gpu_name", "hostname"},
	)

	gpuUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpu_utilization_percent",
			Help: "GPU utilization percentage",
		},
		[]string{"gpu_uuid", "gpu_name", "hostname"},
	)

	gpuPowerUsage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpu_power_usage_watts",
			Help: "GPU power usage in watts",
		},
		[]string{"gpu_uuid", "gpu_name", "hostname"},
	)
	gpuProcessMemoryUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpu_process_memory_used_bytes",
			Help: "GPU memory used by a process in bytes",
		},
		[]string{"gpu_uuid", "gpu_name", "hostname", "pid", "uid", "process_name"},
	)
	k8sNamespacePodCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_namespace_pod_count_total",
			Help: "Total number of pods in each namespace",
		},
		[]string{"namespace", "hostname"},
	)
)

func init() {
	prometheus.MustRegister(gpuTemp)
	prometheus.MustRegister(gpuMemTotal)
	prometheus.MustRegister(gpuMemUsed)
	prometheus.MustRegister(gpuUtilization)
	prometheus.MustRegister(gpuPowerUsage)
	prometheus.MustRegister(gpuProcessMemoryUsed)
	prometheus.MustRegister(k8sNamespacePodCount)
}

func collectMetrics() {
	gpuProcessMemoryUsed.Reset()
	hostname, err := os.Hostname()
    	if err != nil {
        	log.Printf("Error getting hostname: %v", err)
        	hostname = "unknown"
    	}
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		log.Printf("Error getting device count: %v", nvml.ErrorString(ret))
		return
	}

	for i := 0; i < int(count); i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			log.Printf("Error getting device handle for index %d: %v", i, nvml.ErrorString(ret))
			continue
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			log.Printf("Error getting UUID for device %d: %v", i, nvml.ErrorString(ret))
			continue
		}
		name, _ := device.GetName()

		temp, ret := device.GetTemperature(nvml.TEMPERATURE_GPU)
		if ret == nvml.SUCCESS {
			gpuTemp.WithLabelValues(uuid, name, hostname).Set(float64(temp))
		}

		memInfo, ret := device.GetMemoryInfo()
		if ret == nvml.SUCCESS {
			gpuMemTotal.WithLabelValues(uuid, name, hostname).Set(float64(memInfo.Total))
			gpuMemUsed.WithLabelValues(uuid, name, hostname).Set(float64(memInfo.Used))
		}

		util, ret := device.GetUtilizationRates()
		if ret == nvml.SUCCESS {
			gpuUtilization.WithLabelValues(uuid, name, hostname).Set(float64(util.Gpu))
		}

		power, ret := device.GetPowerUsage()
		if ret == nvml.SUCCESS {
			gpuPowerUsage.WithLabelValues(uuid, name, hostname).Set(float64(power) / 1000.0)
		}
		procs, ret := device.GetComputeRunningProcesses()
		if ret != nvml.SUCCESS {
			continue
		}
		for _, p := range procs {
			pid := p.Pid
			uid := "N/A"
			procName := "N/A"
			proc, err := process.NewProcess(int32(pid))
			if err == nil {
				procName, _ = proc.Name()
				uids, err := proc.Uids()
				if err == nil && len(uids) > 0 {
					uid = fmt.Sprint(uids[0]) 
				}
			}
			gpuProcessMemoryUsed.WithLabelValues(
				uuid,
				name,
				hostname,
				fmt.Sprint(pid),
				uid,
				procName,
			).Set(float64(p.UsedGpuMemory))
		}
	}
	if clientset != nil {
		k8sNamespacePodCount.Reset()
		pods, err := clientset.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
		if err == nil {
			podCountByNamespace := make(map[string]int)
			for _, pod := range pods.Items {
				podCountByNamespace[pod.Namespace]++
			}
			for namespace, count := range podCountByNamespace {
				k8sNamespacePodCount.WithLabelValues(namespace, hostname).Set(float64(count))
			}
		} else {
			log.Printf("Failed to get K8s pods info: %v", err)
		}
	}
	log.Println("Metrics collected.")
}

func getLocalIP() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() && ip.To4() != nil && strings.HasPrefix(ip.String(), "192.168.") {
				return ip.String(), nil
			}
		}
	}
	return "", errors.New("can't find available local ip address")
}
var clientset *kubernetes.Clientset
func main() {
	port := flag.Int("port", 8080, "metrics port to listen on")
	flag.Parse()

	localIP, err := getLocalIP()
	if err != nil {
		log.Fatalf("Failed to get local IP: %v", err)
	}

	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		log.Fatalf("Failed to initialize NVML: %v", nvml.ErrorString(ret))
	}
	defer nvml.Shutdown()
	
	kubeconfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
    	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
    	if err != nil {
        	log.Printf("Failed to load Kubeconfig: %v ", err)
    	} else {
        	clientset, err = kubernetes.NewForConfig(config)
        	if err != nil {
            		log.Printf("Failed to create K8s clientset: %v", err)
        	}
    	}
	go func() {
		for {
			collectMetrics()
			time.Sleep(5 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	listenAddr := fmt.Sprintf("%s:%d", localIP, *port)
	log.Printf("Exporter starting on http://%s/metrics", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}
