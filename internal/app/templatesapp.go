package app

import "github.com/nofuturekid/tasmofleet/internal/ota"

// templateApplier is what the OTA engine uses to run a GPIO template job, or nil (a nil interface, not a
// typed nil) when the service is not set up.
func (a *runtime) templateApplier() ota.TemplateApplier {
	if a.templates == nil {
		return nil
	}
	return a.templates.NewApplier()
}
