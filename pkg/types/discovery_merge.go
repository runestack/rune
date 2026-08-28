package types

// MergeServiceDiscovery applies operator discovery fields from updated
// onto the persisted record, always keeping the control-plane VIP.
func MergeServiceDiscovery(existing, updated *ServiceDiscovery) *ServiceDiscovery {
	if existing == nil && updated == nil {
		return nil
	}
	out := &ServiceDiscovery{}
	if existing != nil {
		*out = *existing
	}
	if updated != nil {
		if updated.Mode != "" {
			out.Mode = updated.Mode
		}
		if updated.LocalityPreference != "" {
			out.LocalityPreference = updated.LocalityPreference
		}
	}
	if out.Mode == "" && out.LocalityPreference == "" && out.VIP == "" {
		return nil
	}
	return out
}
