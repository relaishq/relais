// The rehearsal adapter uses the same operator session through NewLocalPeer or
// Adapter.Serve over P1 pipes. It does not define a separate rehearsal gate loop.
//
// Setup reads Load(os.DirFS(checkout), config), obtains an actual allowed Linux
// CPU mask with DetectLayout, and creates a Deployment with a pre-fetched release
// archive. Callers must defer Deployment.Close with a fresh bounded context even
// if Boot fails. Boot verifies the archive, creates local TLS certificates and a
// TLS-only Valkey cluster, and boots the four template hosts. NegativeControls
// checks credential/unit/source refusal; the wrong-ACL control runs during Boot
// before the setup-only default store user is disabled. Both must complete before
// any gate starts. Gate/session results still come only from operatorgates.
// Cleanup requires ownership and selects full 64-hex container IDs with
// --no-trunc. Execution and network attachments use confirmed creation IDs.
// Store data uses tmpfs; no image or volume is removed. Images remain build cache.
//
// P6 supplies P4's caller and directory mailbox through the small Caller and
// Mailbox interfaces. They receive the original operator.Request and return an
// operator.Reply with typed transport certainty and unchanged operation evidence.
// Adapter.Containers must receive Deployment.ContainerIDs after successful Boot;
// missing or abbreviated bindings refuse execution before submission.
// Host results use operator.HostOutput: the P2 ingress decodes Stdout with its own host
// envelope/product decoders. No product status is rebuilt here. Release reads
// use artifacts.NewPublicReader from P2, with an explicit step-scoped job token.
//
// DriverCommand pins the driver to Layout.Services. P4 must pin the caller unit,
// bridge and decoder to Layout.Caller with GOMAXPROCS=2. P6 must record topology,
// process affinities, saturation and decoder pressure. Container pinning alone
// does not prove physical-core isolation. SignalingURL uses the container IP,
// covered by the generated certificate, so no runner DNS or hosts-file change
// is needed. Trust Work/tls/signal/ca.crt, or pin the leaf certificate as P4 needs.
//
// The manual test-operator-rehearsal-container target tests template boot and
// negative controls on Linux Docker. Ordinary tests validate offline logic only.
// They are not evidence of container execution or a complete four-gate media run.
package operatorrehearsal
