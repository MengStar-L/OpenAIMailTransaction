package app

// Country availability and spending limits are live administrator policy for
// every new phone purchase, including replacements on already-issued vouchers.
// The voucher's service and waiting time remain part of its original contract;
// allocated orders retain their own resource, deadline and channel snapshot.
// Callers hold providerGate through lookup/purchase so a settings save cannot
// overtake a purchase validated against the preceding policy.
func (a *App) effectivePhoneSettings(snapshot Settings) Settings {
	current := a.currentSettings()
	snapshot.PhoneCountry = current.PhoneCountry
	snapshot.PhoneMaxPrice = current.PhoneMaxPrice
	return snapshot
}
