package nvtop

// #cgo CFLAGS: -I . -DUSING_LIBSYSTEMD=1 -isystem /usr/include/libdrm/
// #cgo LDFLAGS: -lm -lsystemd -ldrm
// #include <gogpu.h>
import "C"
import (
	"errors"
	"unsafe"
)

type GpuInfo struct {
	Name            string
	Integrated      bool
	Gpu_util_rate   int32 // % of max utilization
	Gpu_clock_speed int32
	Mem_clock_speed int32
	Total_memory    int64
	Used_memory     int64
	Pcie_speed      int32
	Pcie_width      int32
	Fan_rate        int32 // % of max rate.
	Gpu_temp        int32
}

/*
struct gogpu_info {
  char *name; // do not free, deinit will clear these.
  bool integrated;
  int32_t gpu_util_rate; // % of max utilization
  int32_t gpu_clock_speed;
  int32_t mem_clock_speed;
  int64_t total_memory;
  int64_t used_memory;
  int32_t pcie_gen;
  int32_t pcie_width;
  int32_t fan_rate; // % of max rate.
  int32_t gpu_temp;
};
*/

func Init() (int32, error) {
	var gpus C.uint32_t = 0
	var err error = nil
	if !C.gogpu_init(&gpus) {
		err = errors.New("Failed to initialize nvtop")
	}
	return int32(gpus), err
}

func Deinit() {
	C.gogpu_deinit()
}

func Fetch() {
	C.gogpu_fetch()
}

var genToSpeed = map[int]int{
	0: 3,
	1: 3,
	2: 5,
	3: 8,
	4: 16,
	5: 32,
}

func Fill(num int32) []GpuInfo {
	if num < 1 {
		return nil
	}

	info := make([]C.struct_gogpu_info, num, num)
	filled := C.gogpu_fill((*C.struct_gogpu_info)(unsafe.Pointer(&info[0])), C.int32_t(len(info)))
	if int(filled) < len(info) {
		panic("Failed to fill all gpus request")
	}
	ret := []GpuInfo{}
	for i := 0; i < int(filled); i++ {
		speed := 3
		if x, ok := genToSpeed[int(info[i].pcie_gen)]; ok {
			speed = x
		}
		ret = append(ret, GpuInfo{
			C.GoString(info[i].name),
			bool(info[i].integrated),
			int32(info[i].gpu_util_rate),
			int32(info[i].gpu_clock_speed),
			int32(info[i].mem_clock_speed),
			int64(info[i].total_memory),
			int64(info[i].used_memory),
			int32(info[i].pcie_width),
			int32(speed),
			int32(info[i].fan_rate),
			int32(info[i].gpu_temp),
		})
	}
	return ret
}
