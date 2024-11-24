// Build nvtop with gpuinfo library patch into ./build, then build with
// `gcc json.c -I include/ -L ./build/src/ -Wl,-rpath ./build/src -lgpuinfo -o
// gpuinfo_test`

#include <stdio.h>
#include <stdlib.h>

#include "gogpu.h"
#include "list.h"
#include "nvtop/extract_gpuinfo.h"

unsigned allDevCount = 0;
LIST_HEAD(monitoredGpus);

bool gogpu_init(uint32_t *num_gpu) {
  bool res = gpuinfo_init_info_extraction(&allDevCount, &monitoredGpus);
  if (res) {
    if (num_gpu) {
      *num_gpu = allDevCount;
    }
    gpuinfo_populate_static_infos(&monitoredGpus);
  }
  return res && allDevCount <= 16;
}

void gogpu_fetch() {
  gpuinfo_refresh_dynamic_info(&monitoredGpus);
  gpuinfo_refresh_processes(&monitoredGpus);
  gpuinfo_utilisation_rate(&monitoredGpus);
  gpuinfo_fix_dynamic_info_from_process_info(&monitoredGpus);
}

int32_t gogpu_fill(struct gogpu_info *info, int32_t size) {
  struct gpu_info *gpuinfo;
  int32_t i = 0;
  list_for_each_entry(gpuinfo, &monitoredGpus, list) {
    if (i >= size) {
      break;
    }
    if (GPUINFO_STATIC_FIELD_VALID(&gpuinfo->static_info, device_name)) {
      info[i].name = gpuinfo->static_info.device_name;
    }
    info[i].integrated = gpuinfo->static_info.integrated_graphics;
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, gpu_util_rate)) {
      info[i].gpu_util_rate = gpuinfo->dynamic_info.gpu_util_rate;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, gpu_clock_speed)) {
      info[i].gpu_clock_speed = gpuinfo->dynamic_info.gpu_clock_speed;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, mem_clock_speed)) {
      info[i].mem_clock_speed = gpuinfo->dynamic_info.mem_clock_speed;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, total_memory)) {
      info[i].total_memory = gpuinfo->dynamic_info.total_memory;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, used_memory)) {
      info[i].used_memory = gpuinfo->dynamic_info.used_memory;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, pcie_link_gen)) {
      info[i].pcie_gen = gpuinfo->dynamic_info.pcie_link_gen;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, pcie_link_width)) {
      info[i].pcie_width = gpuinfo->dynamic_info.pcie_link_width;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, fan_speed)) {
      info[i].fan_rate = gpuinfo->dynamic_info.fan_speed;
    }
    if (GPUINFO_DYNAMIC_FIELD_VALID(&gpuinfo->dynamic_info, gpu_temp)) {
      info[i].gpu_temp = gpuinfo->dynamic_info.gpu_temp;
    }
	i++;
  }
  return i;
  /*
    for (int i = 0; i < gpuinfo->processes_count; i++) {
      bool using = false;
      struct gpu_process *p = &gpuinfo->processes[i];
      if (GPUINFO_PROCESS_FIELD_VALID(p, gfx_engine_used)) {
        using = true;
        printf("Proccess gfx : %ld us\n", p->gfx_engine_used);
      }
      if (GPUINFO_PROCESS_FIELD_VALID(p, compute_engine_used)) {
        using = true;
        printf("Proccess compute : %ld us\n", p->compute_engine_used);
      }
      if (GPUINFO_PROCESS_FIELD_VALID(p, enc_engine_used)) {
        using = true;
        printf("Proccess enc: %ld us\n", p->enc_engine_used);
      }
      if (GPUINFO_PROCESS_FIELD_VALID(p, dec_engine_used)) {
        using = true;
        printf("Proccess enc: %ld us\n", p->dec_engine_used);
      }
      if (using) {
        printf("Proccess(%d): %s\n", p->pid, p->cmdline);
      }
    }
    // gpu_clock_speed;
    // mem_clock_speed;
    // gpu_util_rate;
    // mem_util_rate; // just computed from _memory values.
    // encoder_rate; // nvidia only
    // decoder_rate; // nvidia/v3d only
    // total_memory;
    // used_memory;
    // free_memory; // Computed from above.
    // static.integrated_graphics;
    // pcie_link_gen;
    // pcie_link_width;
    // fan_speed; // Percentage
    // gpu_temp;
    // power_draw;
  }
  // }
  */
}

void gogpu_deinit() { gpuinfo_shutdown_info_extraction(&monitoredGpus); }

/*
int main(int argc, char **argv) {

  if (!gogpu_init(NULL))
    return EXIT_FAILURE;
  if (allDevCount == 0) {
    fprintf(stdout, "No GPU to monitor.\n");
    return EXIT_SUCCESS;
  }

  gogpu_fetch();
  struct gogpu_info gi[1];
  gogpu_fill(gi, 1);
  printf("name: %s\n", gi[0].name);


  gogpu_deinit();
}
*/
