package observe

import (
	"reflect"
)

// EvidenceOptional is the single exception ledger for raw media evidence.
// Every unlisted exported JSON field is required, even with omitempty.
// Exceptions allow omission/null only; supplied objects are checked recursively.
func EvidenceOptional(typ reflect.Type, field reflect.StructField, object map[string]any) string {
	key := typ.String() + "." + field.Name
	if rule, ok := evidenceConditionalOptional[key]; ok {
		if rule.applies(object) {
			return rule.reason
		}
		return ""
	}
	return evidenceOptional[key]
}

type optionalEvidenceRule struct {
	reason  string
	applies func(map[string]any) bool
}

var evidenceConditionalOptional = map[string]optionalEvidenceRule{
	"controlplane.MoveResult.Start": {"Historical takeovers may omit start; qualification uses independent signal timing.", func(object map[string]any) bool { return object["kind"] == "takeover" }},
	"loadgen.Message.Call":          {"Call payload belongs only to call messages.", func(object map[string]any) bool { return object["type"] != "call" }},
	"loadgen.Message.Stats":         {"Stats payload belongs only to stats messages.", func(object map[string]any) bool { return object["type"] != "stats" }},
	"loadgen.Message.Dial":          {"Dial payload belongs only to dial messages.", func(object map[string]any) bool { return object["type"] != "dial" }},
	"loadgen.Message.Session":       {"Only call and dial messages have session identities.", func(object map[string]any) bool { return object["type"] != "call" && object["type"] != "dial" }},
	"callharness.TrackReport.Video": {"Only audio tracks may omit video evidence.", func(object map[string]any) bool { return object["Kind"] == "audio" }},
}

// EvidenceOptionalLedger exposes a copy for exhaustive coverage of every
// exception, including those whose applicability depends on sibling fields.
func EvidenceOptionalLedger() map[string]string {
	fields := make(map[string]string, len(evidenceOptional)+len(evidenceConditionalOptional))
	for key, reason := range evidenceOptional {
		fields[key] = reason
	}
	for key, rule := range evidenceConditionalOptional {
		fields[key] = rule.reason
	}
	return fields
}

var evidenceOptional = map[string]string{
	"main.previewMediaInput.Gate":                      "Legacy inputs require trial collections; only an explicit relay-deploy gate can contribute a call without trials.",
	"operatorevidence.MediaInput.Gate":                 "Legacy inputs require trial collections; only an explicit relay-deploy gate can contribute a call without trials.",
	"loadgen.Message.CallID":                           "The product omits call index zero; inventory joins validate the decoded index.",
	"loadgen.Message.Error":                            "Only error messages carry error text; they cannot qualify.",
	"loadgen.Message.MediaOffset":                      "Optional scheduling diagnostic, not a judge bound.",
	"loadgen.Message.MaintenancePhase":                 "Optional maintenance diagnostic, not a judge bound.",
	"main.previewMediaInput.Missing":                   "False phase provenance flags are omitted by the bridge.",
	"operatorevidence.MediaInput.Missing":              "False phase provenance flags are omitted by the bridge.",
	"callharness.DistributedMeasurement.KillToLive":    "Only takeovers have this bound; callCollections requires it for takeovers.",
	"loadgen.EventResult.Failed":                       "The product omits false event failures; event joins and outcomes are recomputed.",
	"loadgen.EventResult.Error":                        "Only failed scheduling operations carry diagnostic error text.",
	"loadgen.Stats.RuntimeMemory":                      "Platform-specific memory diagnostics do not set media or generator thresholds.",
	"loadgen.Stats.PhysicalFootprintBytes":             "Platform-specific memory diagnostic is omitted when unavailable.",
	"loadgen.Stats.RSSBytes":                           "Platform-specific memory diagnostic is omitted when unavailable.",
	"loadgen.Stats.HeapInUseBytes":                     "Optional memory diagnostic does not set generator thresholds.",
	"controlplane.MoveResult.SnapshotAge":              "Optional server checkpoint diagnostic, not a caller bound.",
	"controlplane.MoveResult.CheckpointAge":            "Optional server checkpoint diagnostic, not a caller bound.",
	"controlplane.MoveResult.CheckpointPolicy":         "Optional checkpoint policy diagnostic does not qualify media.",
	"controlplane.MoveResult.SequenceMargin":           "Optional server sequence diagnostic does not qualify media.",
	"controlplane.MoveResult.SRTCPIndexMargin":         "Optional server index diagnostic does not qualify media.",
	"controlplane.MoveResult.DetectedAt":               "Only takeover results have a detection timestamp.",
	"controlplane.MoveResult.LastHeartbeat":            "Only takeover results have a heartbeat timestamp.",
	"controlplane.MoveResult.LossCause":                "Only lost operations carry a loss cause.",
	"controlplane.MoveResult.Lost":                     "The product omits false loss flags; a positive flag independently fails.",
	"controlplane.MoveResult.Error":                    "The product omits successful operation error text.",
	"mediaworker.HandoverResult.MoveDiagnosticID":      "Optional correlation diagnostic does not qualify media.",
	"mediaworker.HandoverResult.DuplicateWindows":      "Optional server duplicate diagnostics do not define caller populations.",
	"main.receipt.Codes":                               "Successful receipts may have no diagnostic codes; codes cannot qualify alone.",
	"operatorcaller.Receipt.HTTPStatus":                "The bridge omits zero HTTP status for successful operations.",
	"operatorcaller.Receipt.ResponseError":             "Successful bridge receipts omit transport error text.",
	"main.receipt.HTTPStatus":                          "The bridge omits zero HTTP status for successful operations.",
	"main.receipt.ResponseError":                       "Successful bridge receipts omit transport error text.",
	"operatorcaller.Receipt.Codes":                     "Successful receipts may have no diagnostic codes; codes cannot qualify alone.",
	"callharness.Report.ReportBuildDuration":           "Optional snapshot cost diagnostic does not qualify media.",
	"callharness.DistributedRecording.ObservedRate":    "Observed drift is diagnostic; required raw probes own clock evidence.",
	"callharness.DistributedRecording.Missing":         "The product omits an empty missing-receipt inventory; any entries disqualify.",
	"callharness.DistributedRecording.ClockError":      "Only failed clock sampling emits diagnostic error text.",
	"callharness.DistributedRecording.Causes":          "Descriptive causes are recomputed from required evidence.",
	"callharness.RecordingReport.Delivery":             "Optional whole-call delivery diagnostics do not qualify event populations.",
	"callharness.ContentDelivery.LostAt":               "A capped diagnostic inventory cannot replace authoritative content counts.",
	"callharness.AnswerFacts.Bundle":                   "SDP diagnostic arrays do not set operator media thresholds.",
	"callharness.AnswerFacts.Candidates":               "SDP diagnostic arrays do not set operator media thresholds.",
	"callharness.AnswerFacts.Media":                    "SDP diagnostic arrays do not set operator media thresholds.",
	"callharness.AnswerMedia.Formats":                  "SDP diagnostic arrays do not set operator media thresholds.",
	"callharness.AnswerMedia.Codecs":                   "SDP diagnostic arrays do not set operator media thresholds.",
	"callharness.AnswerMedia.RTCPFeedback":             "SDP diagnostic arrays do not set operator media thresholds.",
	"callharness.Report.ConnectionStates":              "Diagnostic state history cannot replace required event evidence.",
	"callharness.Report.ICEConnectionStates":           "Diagnostic state history cannot replace required event evidence.",
	"callharness.Report.SRTPDiagnostics":               "Optional packet diagnostics supplement authoritative decryption counters.",
	"callharness.SRTPDiagnosticReport.Unavailable":     "Only unavailable diagnostics carry a reason.",
	"callharness.SRTPDiagnosticReport.Samples":         "Optional capped packet samples cannot replace authoritative counters.",
	"callharness.SRTPFailureSample.Sequence":           "RTP-only diagnostic omits zero values.",
	"callharness.SRTPFailureSample.Timestamp":          "RTP-only diagnostic omits zero values.",
	"callharness.SRTPFailureSample.PayloadType":        "RTP-only diagnostic omits zero values.",
	"callharness.SRTPFailureSample.AuthenticatedIndex": "Authentication failures cannot supply an authenticated index.",
	"callharness.TrackReport.MediaGaps":                "Sparse capped diagnostics cannot replace required distributed gap populations.",
	"callharness.TrackReport.Arrivals":                 "Rolling packet history cannot replace required distributed gap populations.",
	"callharness.MoveReport.Receipt":                   "Server receipts are mandatory separately; supplied caller copies must match.",
	"callharness.DistributedEvent.Uncertain":           "The product omits false caller receipt uncertainty; the server flag is required.",
	"callharness.MetricVerdict.Reasons":                "Descriptive reasons do not set thresholds; Trusted and Failed are required.",
	"callharness.DistributedGap.Causes":                "Descriptive causes are recomputed from required gap evidence.",
	"callharness.DistributedMeasurement.Causes":        "Descriptive causes are recomputed from required event evidence.",
	"callharness.MoveReport.Lost":                      "The product omits false loss flags; a positive flag independently fails.",
	"callharness.MoveReport.LossCause":                 "Only lost moves carry descriptive loss text.",
	"callharness.VideoRecovery.CachedPictureIDs":       "Capped picture diagnostics do not set operator populations.",
	"callharness.ContentLossEvidence.Baselines":        "No exported completeness count; exclusions cover the full caller span.",
	"callharness.EventMeasurement.Reasons":             "Descriptive reasons do not set metric thresholds.",
	"callharness.MoveReport.Tracks":                    "Legacy per-track diagnostics do not replace required distributed gaps.",
	"callharness.DialReport.Failure":                   "Only failed dials carry a failure category.",
	"callharness.DialReport.Error":                     "Only failed dials carry error text.",
	"callharness.DialReport.ICEStates":                 "Optional dial diagnostics do not replace the connected outcome and session.",
	"callharness.DialReport.DTLSStates":                "Optional dial diagnostics do not replace the connected outcome and session.",
	"callharness.DialReport.ConnectionStates":          "Optional dial diagnostics do not replace the connected outcome and session.",
	"callharness.DialReport.CandidatePairs":            "Optional dial diagnostics do not replace the connected outcome and session.",
	"callharness.CandidatePairChecks.FirstCheck":       "The product omits absent check timestamps; diagnostic only.",
	"callharness.CandidatePairChecks.LastCheck":        "The product omits absent check timestamps; diagnostic only.",
	"callharness.CandidatePairChecks.LastResponse":     "The product omits absent response timestamps; diagnostic only.",
}
