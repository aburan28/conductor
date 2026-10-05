package admin

// Feature is an advanced area an organization can turn on. Features decide what the
// dashboard shows and what clients offer; they are not access controls — every API route
// keeps its own authorization whatever the flag says — so turning one off simplifies the
// product without breaking a script or a worker that already uses it.
type Feature struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Features lists every feature flag. A new organization starts with all of them off: the
// simple product is check before edit, claims, conflicts, and merge.
var Features = []Feature{
	{Key: "queue", Label: "Admission queue",
		Description: "Sessions and runner attempts wait in line when the project is at its concurrency cap. Shows the Queue page."},
	{Key: "swarm", Label: "Swarm",
		Description: "Pooled capacity across teammates' runners and sessions. Shows the Swarm page."},
	{Key: "budget_sharing", Label: "Budget sharing",
		Description: "Members can give part of their token allowance to a teammate. Shows the share controls."},
	{Key: "mesh", Label: "Mesh peering",
		Description: "Daemon-to-daemon links across machines or teams. Shows peer status on the Fleet page."},
	{Key: "local_models", Label: "Local model serving",
		Description: "Self-hosted models (conductor serve) in the model catalog and integration snippets."},
	{Key: "checkpoints_by_account", Label: "Checkpoints by account",
		Description: "Resuming a saved session under another login when one hits its usage limit."},
}

// FeatureByKey returns the feature with this key, or nil.
func FeatureByKey(key string) *Feature {
	for i := range Features {
		if Features[i].Key == key {
			return &Features[i]
		}
	}
	return nil
}

// FeatureKeys lists the keys in display order.
func FeatureKeys() []string {
	out := make([]string, len(Features))
	for i, f := range Features {
		out[i] = f.Key
	}
	return out
}

// AllFeaturesOn is the feature set of an organization that existed before feature flags:
// everything it could already see stays visible.
func AllFeaturesOn() map[string]bool {
	out := map[string]bool{}
	for _, f := range Features {
		out[f.Key] = true
	}
	return out
}
