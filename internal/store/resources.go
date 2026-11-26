package store

// ContainerResources limits the container's entire job, including exec children.
// Zero means unlimited. NanoCPUs is a number of logical CPUs in billionths.
type ContainerResources struct {
	NanoCPUs  int64 `json:"NanoCpus"`
	Memory    int64 `json:"Memory"`
	PidsLimit int64 `json:"PidsLimit"`
}

type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

func (p RestartPolicy) Enabled() bool { return p.Name != "" && p.Name != "no" }
