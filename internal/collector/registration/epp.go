package registration

// schedulerDispatchRate selects one metric family per EPP source before aggregation.
// Normalize the endpoint label so both families identify the same serving pod.
const schedulerDispatchRate = `label_replace(rate(llm_d_epp_scheduler_attempts_total{status="success",namespace="{{.namespace}}",target_model_name="{{.modelID}}"}[1m]), "pod_name", "$1", "endpoint_name", "(.+)")` +
	` or ignoring(endpoint_name) rate(inference_extension_scheduler_attempts_total{status="success",namespace="{{.namespace}}",target_model_name="{{.modelID}}"}[1m])`

// schedulerQueue preserves model-name fallback and selects the current family
// before summing, including during rolling upgrades with legacy-only sources.
func schedulerQueue(suffix string) string {
	current := "llm_d_epp_flow_control_queue_" + suffix
	legacy := "inference_extension_flow_control_queue_" + suffix
	return `sum(` + current + `{target_model_name="{{.modelID}}"} or ` + legacy + `{target_model_name="{{.modelID}}"})` +
		` or sum(` + current + `{model_name="{{.modelID}}",target_model_name=""} or ` + legacy + `{model_name="{{.modelID}}",target_model_name=""})`
}
