#include <stdbool.h>
#include <stdint.h>

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

bool gogpu_init(uint32_t *num_gpu);
void gogpu_fetch();
int32_t gogpu_fill(struct gogpu_info *info, int32_t size);
void gogpu_deinit();
