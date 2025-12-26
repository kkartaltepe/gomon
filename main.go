package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gomon/nvtop"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	mpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/anatol/smart.go"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	netps "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
)

var metricInterval = flag.Duration("interval", 10*time.Second, "Interval between reporting metrics")
var otelEndpoint = flag.String("endpoint", "localhost:4318", "The host:port to use for the OpenTelemetry collector")

type deviceUsage struct {
	part  *disk.PartitionStat
	usage *disk.UsageStat
}

func getUsages(ctx context.Context, parts []disk.PartitionStat) ([]deviceUsage, error) {
	usages := make([]deviceUsage, 0, len(parts))

	type mountKey struct {
		mountpoint string
		device     string
	}
	seen := map[mountKey]struct{}{}

	for _, partition := range parts {
		key := mountKey{
			mountpoint: partition.Mountpoint,
			device:     partition.Device,
		}
		if _, ok := seen[key]; partition.Mountpoint != "" && ok {
			continue
		}
		seen[key] = struct{}{}

		usage, err := disk.UsageWithContext(ctx, partition.Mountpoint)
		if err != nil {
			return nil, err
		}

		usages = append(usages, deviceUsage{&partition, usage})
	}

	if len(usages) < 1 {
		return nil, nil
	}
	return usages, nil
}

func recordCPU(now uint64, cpuStats []cpu.TimesStat) []*mpb.Metric {
	timeMetric := &mpb.Metric{
		Name:        "system.cpu.time",
		Description: "Total seconds each logical CPU spent on each mode.",
		Unit:        "seconds",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	freqMetric := &mpb.Metric{
		Name:        "system.cpu.frequency",
		Description: "Current frequenecy of the CPU in Hz",
		Unit:        "hz",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	for _, cpuStat := range cpuStats {
		timeMetric.GetSum().DataPoints = append(timeMetric.GetSum().DataPoints,
			dataF(now, cpuStat.User, []string{"cpu", cpuStat.CPU, "state", "user"}),
			dataF(now, cpuStat.System, []string{"cpu", cpuStat.CPU, "state", "system"}),
			dataF(now, cpuStat.Idle, []string{"cpu", cpuStat.CPU, "state", "idle"}),
			dataF(now, cpuStat.Irq, []string{"cpu", cpuStat.CPU, "state", "interrupt"}),
			dataF(now, cpuStat.Nice, []string{"cpu", cpuStat.CPU, "state", "nice"}),
			dataF(now, cpuStat.Softirq, []string{"cpu", cpuStat.CPU, "state", "softirq"}),
			dataF(now, cpuStat.Steal, []string{"cpu", cpuStat.CPU, "state", "steal"}),
			dataF(now, cpuStat.Iowait, []string{"cpu", cpuStat.CPU, "state", "iowait"}),
		)

		// hz from below
		b, err := os.ReadFile("/sys/devices/system/cpu/" + cpuStat.CPU + "/cpufreq/scaling_cur_freq")
		if err != nil {
			panic(err)
		}
		// cur_freq is in KHz, trim newline
		hz, _ := strconv.ParseInt(string(b[:len(b)-1]), 10, 64)
		hz = hz * 1000
		freqMetric.GetGauge().DataPoints = append(freqMetric.GetGauge().DataPoints,
			dataI(now, hz, []string{"cpu", cpuStat.CPU}),
		)
	}
	return []*mpb.Metric{timeMetric, freqMetric}
}

var fsModeFromOpt = func(opts []string) string {
	if slices.Contains(opts, "rw") {
		return "rw"
	}
	if slices.Contains(opts, "ro") {
		return "ro"
	}
	return "unknown"
}

func recordFS(now uint64, dus []deviceUsage) []*mpb.Metric {
	fsMetric := &mpb.Metric{
		Name:        "system.filesystem.usage",
		Description: "Filesystem bytes used.",
		Unit:        "byte",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	for _, du := range dus {
		attrs := []string{
			"device", du.part.Device,
			"mountpoint", du.part.Mountpoint,
			"mode", fsModeFromOpt(du.part.Opts),
			"type", du.part.Fstype,
		}
		fsMetric.GetGauge().DataPoints = append(fsMetric.GetGauge().DataPoints,
			dataI(now, int64(du.usage.Total), append(attrs, "state", "total")),
			dataI(now, int64(du.usage.Used), append(attrs, "state", "used")),
			dataI(now, int64(du.usage.Free), append(attrs, "state", "free")),
		)
	}
	return []*mpb.Metric{fsMetric}
}

func recordDisk(now uint64, ioCounters map[string]disk.IOCountersStat) []*mpb.Metric {
	ioMetric := &mpb.Metric{
		Name:        "system.disk.io",
		Description: "Disk bytes transferred.",
		Unit:        "byte",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	opsMetric := &mpb.Metric{
		Name:        "system.disk.operations",
		Description: "Disk operations count.",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	for device, ioCounter := range ioCounters {
		ioMetric.GetSum().DataPoints = append(ioMetric.GetSum().DataPoints,
			dataI(now, int64(ioCounter.ReadBytes), []string{"device", "/dev/" + device, "direction", "read"}),
			dataI(now, int64(ioCounter.WriteBytes), []string{"device", "/dev/" + device, "direction", "write"}),
		)
		opsMetric.GetSum().DataPoints = append(opsMetric.GetSum().DataPoints,
			dataI(now, int64(ioCounter.ReadCount), []string{"device", "/dev/" + device, "direction", "read"}),
			dataI(now, int64(ioCounter.WriteCount), []string{"device", "/dev/" + device, "direction", "write"}),
		)
	}
	return []*mpb.Metric{ioMetric, opsMetric}
	/*
		for device, ioCounter := range ioCounters {
			s.mb.RecordSystemDiskPendingOperationsDataPoint(now, int64(ioCounter.IopsInProgress), device)
		}
	*/
}

func recordNet(now uint64, ioCounters []netps.IOCountersStat) []*mpb.Metric {
	ioMetric := &mpb.Metric{
		Name:        "system.network.io",
		Description: "The number of bytes transmitted and received.",
		Unit:        "byte",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	opsMetric := &mpb.Metric{
		Name:        "system.network.packets",
		Description: "The number of packets transferred.",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	dropMetric := &mpb.Metric{
		Name:        "system.network.dropped",
		Description: "The number of packets dropped",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	for _, ioCounter := range ioCounters {
		ioMetric.GetSum().DataPoints = append(ioMetric.GetSum().DataPoints,
			dataI(now, int64(ioCounter.BytesRecv), []string{"device", ioCounter.Name, "direction", "receive"}),
			dataI(now, int64(ioCounter.BytesSent), []string{"device", ioCounter.Name, "direction", "transmit"}),
		)
		opsMetric.GetSum().DataPoints = append(opsMetric.GetSum().DataPoints,
			dataI(now, int64(ioCounter.PacketsRecv), []string{"device", ioCounter.Name, "direction", "receive"}),
			dataI(now, int64(ioCounter.PacketsSent), []string{"device", ioCounter.Name, "direction", "transmit"}),
		)
		dropMetric.GetSum().DataPoints = append(dropMetric.GetSum().DataPoints,
			dataI(now, int64(ioCounter.Dropin), []string{"device", ioCounter.Name, "direction", "receive"}),
			dataI(now, int64(ioCounter.Dropout), []string{"device", ioCounter.Name, "direction", "transmit"}),
		)
	}
	return []*mpb.Metric{ioMetric, opsMetric, dropMetric}
}

// Base devices, not including partitions/namespaces/etc.
// Actually use namespaces as targets because disk only grants access to namespaces and higher.
var isDev = regexp.MustCompile("^(sd[a-z]+|nvme[0-9]+n[0-9]+)$")

func recordSmart(now uint64) []*mpb.Metric {
	smartMetric := &mpb.Metric{
		Name:        "system.disk.smart",
		Description: "Attributes from SMART",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	f, err := os.Open("/dev")
	if err != nil {
		panic(err)
	}
	for names, err := f.Readdirnames(32); len(names) > 0 && err == nil; names, err = f.Readdirnames(32) {
		for _, name := range names {
			if !isDev.MatchString(name) {
				continue
			}
			devPath := "/dev/" + name
			dev, err := smart.Open(devPath)
			if err != nil {
				panic(err)
			}
			defer dev.Close()

			sm, err := dev.ReadGenericAttributes()
			if err != nil {
				panic(err)
			}
			smartMetric.GetGauge().DataPoints = append(smartMetric.GetGauge().DataPoints,
				dataI(now, int64(sm.Temperature), []string{"device", devPath, "name", "temp"}),
				dataI(now, int64(sm.PowerOnHours), []string{"device", devPath, "name", "power_on_time"}),
				dataI(now, int64(sm.PowerCycles), []string{"device", devPath, "name", "power_cycles"}),
				// Fix this shitter also not sending sane read/write data.
			)
		}
	}

	return []*mpb.Metric{smartMetric}
}

func recordPower(now uint64) []*mpb.Metric {
	powerMetric := &mpb.Metric{
		Name:        "system.powercap.energy",
		Description: "Energy usage readings from the powercap subsystem",
		Unit:        "uJ",
		Data: &mpb.Metric_Sum{
			Sum: &mpb.Sum{
				DataPoints:             []*mpb.NumberDataPoint{},
				AggregationTemporality: mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
			},
		},
	}
	f, err := os.Open("/sys/class/powercap/")
	if err != nil {
		panic(err)
	}
	for names, err := f.Readdirnames(32); len(names) > 0 && err == nil; names, err = f.Readdirnames(32) {
		for _, name := range names {
			energy, err := os.ReadFile("/sys/class/powercap/" + name + "/energy_uj")
			if err != nil {
				continue
			}
			energyS := string(energy[:len(energy)-1])
			energyI, err := strconv.ParseInt(energyS, 10, 64)
			if err != nil {
				panic(err)
			}
			if energyI == 0 {
				fmt.Println("invalid energy from powercap: %v", energyS)
				continue
			}
			label, err := os.ReadFile("/sys/class/powercap/" + name + "/name")
			if err != nil {
				panic(err)
			}
			powerMetric.GetSum().DataPoints = append(powerMetric.GetSum().DataPoints,
				dataI(now, energyI, []string{"domain", name, "name", strings.TrimRight(string(label), "\n")}),
			)
		}
	}

	return []*mpb.Metric{powerMetric}
}

func recordMemory(now uint64, memInfo *mem.VirtualMemoryStat) []*mpb.Metric {
	return []*mpb.Metric{{
		Name: "system.memory.usage",
		Unit: "byte",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{
					dataI(now, int64(memInfo.Total), []string{"state", "total"}),
					dataI(now, int64(memInfo.Used), []string{"state", "used"}),
					dataI(now, int64(memInfo.Free), []string{"state", "free"}),
					dataI(now, int64(memInfo.Buffers), []string{"state", "buffered"}),
					dataI(now, int64(memInfo.Cached), []string{"state", "cached"}),
					dataI(now, int64(memInfo.Sreclaimable), []string{"state", "slab_reclaimable"}),
					dataI(now, int64(memInfo.Sunreclaim), []string{"state", "slab_unreclaimable"}),
				},
			},
		},
	}}
}

func recordGPU(now uint64, gpus int32) []*mpb.Metric {
	utilMetric := &mpb.Metric{
		Name:        "system.gpu.usage",
		Description: "Percentage utilization",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	freqMetric := &mpb.Metric{
		Name:        "system.gpu.frequency",
		Description: "Current frequency of gpu clocks",
		Unit:        "MHz",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	memMetric := &mpb.Metric{
		Name:        "system.gpu.memory.usage",
		Description: "Amount of video memory used",
		Unit:        "byte",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	pcieMetric := &mpb.Metric{
		Name:        "system.gpu.pcie",
		Description: "PCIe link width and link speed in GT/s",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	tempMetric := &mpb.Metric{
		Name:        "system.gpu.temp",
		Description: "GPU temperature sensor data",
		Unit:        "C",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	gpuInfos := nvtop.Fill(gpus)
	for _, info := range gpuInfos {
		// TODO: Dont send metrics if there are no stats.
		utilMetric.GetGauge().DataPoints = append(utilMetric.GetGauge().DataPoints,
			dataI(now, int64(info.Gpu_util_rate), []string{"name", info.Name}),
		)
		freqMetric.GetGauge().DataPoints = append(freqMetric.GetGauge().DataPoints,
			dataI(now, int64(info.Gpu_clock_speed), []string{"name", info.Name, "domain", "gfx"}),
			dataI(now, int64(info.Mem_clock_speed), []string{"name", info.Name, "domain", "mem"}),
		)
		memMetric.GetGauge().DataPoints = append(memMetric.GetGauge().DataPoints,
			dataI(now, int64(info.Total_memory), []string{"name", info.Name, "state", "total"}),
			dataI(now, int64(info.Used_memory), []string{"name", info.Name, "state", "used"}),
		)
		pcieMetric.GetGauge().DataPoints = append(pcieMetric.GetGauge().DataPoints,
			dataI(now, int64(info.Pcie_speed), []string{"name", info.Name, "domain", "speed"}),
			dataI(now, int64(info.Pcie_width), []string{"name", info.Name, "domain", "width"}),
		)
		tempMetric.GetGauge().DataPoints = append(tempMetric.GetGauge().DataPoints,
			dataI(now, int64(info.Gpu_temp), []string{"name", info.Name}),
		)
	}

	return []*mpb.Metric{utilMetric, freqMetric, memMetric, pcieMetric, tempMetric}
}

func recordTemps(now uint64, temps []sensors.TemperatureStat) []*mpb.Metric {
	tempMetric := &mpb.Metric{
		Name:        "system.sensor.temp",
		Description: "Readings from temperature sensors",
		Unit:        "C",
		Data: &mpb.Metric_Gauge{
			Gauge: &mpb.Gauge{
				DataPoints: []*mpb.NumberDataPoint{},
			},
		},
	}
	// Remove when gopsutil is updated.
	seen := map[string]bool{}
	for i, _ := range temps {
		name := temps[i].SensorKey
		if _, ok := seen[name]; ok {
			// TODO: increment.
			temps[i].SensorKey = name + "-1"
			if _, ok := seen[temps[i].SensorKey]; ok {
				temps[i].SensorKey = name + "-2"
			}
		}
		seen[temps[i].SensorKey] = true
	}
	for _, temp := range temps {
		tempMetric.GetGauge().DataPoints = append(tempMetric.GetGauge().DataPoints,
			dataF(now, float64(temp.Temperature), []string{"name", temp.SensorKey}),
		)
	}
	return []*mpb.Metric{tempMetric}
}

func pbAttrsS(attrs []string) []*cpb.KeyValue {
	if len(attrs) < 2 {
		return nil
	}

	pb := []*cpb.KeyValue{}
	for i := 0; i < len(attrs)-1; i = i + 2 {
		pb = append(pb, &cpb.KeyValue{Key: attrs[i], Value: &cpb.AnyValue{Value: &cpb.AnyValue_StringValue{StringValue: attrs[i+1]}}})
	}
	return pb
}

func dataI(now uint64, i int64, attrs []string) *mpb.NumberDataPoint {
	return &mpb.NumberDataPoint{
		Attributes:   pbAttrsS(attrs),
		TimeUnixNano: now,
		Value:        &mpb.NumberDataPoint_AsInt{AsInt: i},
	}
}
func dataF(now uint64, f float64, attrs []string) *mpb.NumberDataPoint {
	return &mpb.NumberDataPoint{
		Attributes:   pbAttrsS(attrs),
		TimeUnixNano: now,
		Value:        &mpb.NumberDataPoint_AsDouble{AsDouble: f},
	}
}

func main() {
	// Allow 2 threads (+runtime) and 16MB of ram.
	debug.SetMemoryLimit(16 * 1024 * 1024)
	runtime.GOMAXPROCS(2)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Kill, syscall.SIGTERM, os.Interrupt)
	defer stop()

	gpus, err := nvtop.Init()
	if err != nil {
		panic(err)
	}
	defer nvtop.Deinit()
	_ = gpus

	httpClient := &http.Client{}
	u := &url.URL{
		Scheme: "http",
		Host:   *otelEndpoint,
		Path:   "/v1/metrics",
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), http.NoBody)
	if err != nil {
		panic(err)
	}
	userAgent := "Shitty gopsutil exporter"
	req.Header.Set("User-Agent", userAgent)
	// Only protobuf accepted
	req.Header.Set("Content-Type", "application/x-protobuf")

	SendMetrics := func(metrics *mpb.ResourceMetrics) {
		pbRequest := &colmetricpb.ExportMetricsServiceRequest{
			ResourceMetrics: []*mpb.ResourceMetrics{metrics},
		}
		// sets ctx
		r := req.Clone(ctx)
		body, err := proto.Marshal(pbRequest)
		if err != nil {
			panic(err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := httpClient.Do(r)
		if err != nil {
			panic(err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			panic("Bad response status code: " + string(resp.StatusCode))
		}
		if err := resp.Body.Close(); err != nil {
			panic(err)
		}
		fmt.Println("Sent metrics")
	}
	_ = SendMetrics

	ticker := time.NewTicker(*metricInterval)
L:
	for {
		select {
		case <-ctx.Done():
			stop()
			break L
		case t := <-ticker.C:
			s, _ := sensors.TemperaturesWithContext(ctx)
			m, _ := mem.VirtualMemoryWithContext(ctx)
			c, _ := cpu.TimesWithContext(ctx, true) // Per cpu
			// ci, _ := cpu.Info()     // max frequency ;_;
			dio, _ := disk.IOCountersWithContext(ctx)
			dp, _ := disk.PartitionsWithContext(ctx, false) //  dont include virtual fs.
			dus, _ := getUsages(ctx, dp)
			ns, _ := netps.IOCountersWithContext(ctx, true) // per interface
			nvtop.Fetch()

			_ = s
			_ = c
			_ = m
			_ = dio
			_ = dus
			_ = ns

			now := uint64(t.UnixNano())
			mt := []*mpb.Metric{}
			mt = append(mt, recordCPU(now, c)...)
			mt = append(mt, recordMemory(now, m)...)
			mt = append(mt, recordDisk(now, dio)...)
			mt = append(mt, recordFS(now, dus)...)
			mt = append(mt, recordNet(now, ns)...)
			mt = append(mt, recordTemps(now, s)...)
			// These require CAP_SYS_ADMIN
			mt = append(mt, recordSmart(now)...)
			mt = append(mt, recordPower(now)...)
			mt = append(mt, recordGPU(now, gpus)...)

			rm := &mpb.ResourceMetrics{
				ScopeMetrics: []*mpb.ScopeMetrics{{
					Metrics: mt,
				}},
			}
			_ = rm
			_ = prototext.Format
			SendMetrics(rm)
			// fmt.Println(prototext.Format(rm))
		}
	}
	fmt.Println("Goodbye")
}
