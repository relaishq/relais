package inventory

// ComponentSlot finds a unique component in independently read host inventories.
// Missing or duplicate components never fall back to the current deployment map.
func ComponentSlot(hosts map[string][]string, component string) (string, bool) {
	var found string
	for _, slot := range Slots {
		for _, name := range hosts[slot] {
			if name == component {
				if found != "" {
					return "", false
				}
				found = slot
			}
		}
	}
	return found, found != ""
}
