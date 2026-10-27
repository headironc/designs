package provider

import "strings"

// guardPolicy binds this repository's ScaleSet to its provider and VM identity.
type guardPolicy struct {
	Controller string `json:"controller_id"`
	Repository string `json:"repository_id"`
	ScaleSet   string `json:"scaleset_name"`
	Pool       string `json:"pool_id"`
	Image      string `json:"image_fingerprint"`
}

func (p guardPolicy) valid() bool {
	// GARM's ScaleSet workers send name-entityID as PoolID, not a pool UUID.
	// Preserve the producer's exact bytes; do not normalize or accept a second form.
	return uuid.MatchString(p.Controller) && uuid.MatchString(p.Repository) &&
		p.ScaleSet != "" && !strings.ContainsRune(p.ScaleSet, '\x00') &&
		p.Pool == p.ScaleSet+"-"+p.Repository && digest.MatchString(p.Image)
}
