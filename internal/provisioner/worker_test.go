package provisioner

import "testing"

func TestWorkerLogRedactionRemovesLeaseAndEnrollmentSecrets(t *testing.T) {
	worker := &Worker{token: "provisioner-secret"}
	lease := lease{LeaseID: "lease-secret"}
	lease.Agent.Token = "enrollment-secret"
	message := worker.redact("provisioner-secret lease-secret enrollment-secret", lease)
	if message != "[REDACTED] [REDACTED] [REDACTED]" {
		t.Fatalf("unexpected redaction result: %q", message)
	}
}
