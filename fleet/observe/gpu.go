package observe

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const gpuQuery = "nvidia-smi --query-gpu=index,memory.total,memory.used,memory.free --format=csv,noheader,nounits"

type GPUState struct {
	Index    int `json:"index"`
	TotalMiB int `json:"total_mib"`
	UsedMiB  int `json:"used_mib"`
	FreeMiB  int `json:"free_mib"`
}

// parseGPUs parses csv,noheader,nounits nvidia-smi output, keeping only GPU
// indices present in want (the host's declared inventory), sorted ascending.
func parseGPUs(out string, want []int) ([]GPUState, error) {
	keep := make(map[int]bool, len(want))
	for _, i := range want {
		keep[i] = true
	}
	var res []GPUState
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 4 {
			return nil, fmt.Errorf("unexpected nvidia-smi line %q", line)
		}
		nums := make([]int, 4)
		for i, p := range parts {
			v, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return nil, fmt.Errorf("bad number in %q: %w", line, err)
			}
			nums[i] = v
		}
		if !keep[nums[0]] {
			continue
		}
		res = append(res, GPUState{Index: nums[0], TotalMiB: nums[1], UsedMiB: nums[2], FreeMiB: nums[3]})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Index < res[j].Index })
	return res, nil
}
