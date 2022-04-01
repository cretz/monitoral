package systemworker

import (
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

type HostInfo struct {
	Name string
	Tags []string
}

// Nil if invalid
func HostInfoFromSearchAttributes(v *commonpb.SearchAttributes) *HostInfo {
	var info HostInfo
	if v == nil {
		return nil
	} else if p := v.IndexedFields["monitoral-host"]; p == nil {
		return nil
	} else if err := converter.GetDefaultDataConverter().FromPayload(p, &info.Name); err != nil {
		return nil
	}
	if p := v.IndexedFields["monitoral-tags"]; p == nil {
		return nil
	} else if err := converter.GetDefaultDataConverter().FromPayload(p, &info.Tags); err != nil {
		return nil
	}
	return &info
}

func (h *HostInfo) ToSearchAttributes() map[string]interface{} {
	return map[string]interface{}{
		"monitoral-host": h.Name,
		"monitoral-tags": h.Tags,
	}
}
