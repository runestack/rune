package types

import "testing"

func TestDeriveServiceReason(t *testing.T) {
	cases := []struct {
		name    string
		status  InstanceStatus
		message string
		want    string
	}{
		{
			// Regression: the service is named "greenroom" — "oom" is a
			// substring of "r-oom" — and the real failure is a missing
			// configmap. Must be ConfigMissing, NOT OutOfMemory.
			name:    "greenroom configmap-not-found is ConfigMissing not OutOfMemory",
			status:  InstanceStatusFailed,
			message: "failed to prepare environment variables: envFrom configmap greenroom.greenroom-config: resource configmap/greenroom/greenroom-config not found",
			want:    ServiceReasonConfigMissing,
		},
		{
			name:    "service name containing oom does not false-positive OOM",
			status:  InstanceStatusFailed,
			message: "greenroom failed to launch: runner refused",
			want:    ServiceReasonLaunchFailed,
		},
		{
			name:    "real docker OOMKilled",
			status:  InstanceStatusFailed,
			message: "container OOMKilled (exit 137)",
			want:    ServiceReasonOutOfMemory,
		},
		{
			name:    "kernel out of memory",
			status:  InstanceStatusFailed,
			message: "process killed: out of memory",
			want:    ServiceReasonOutOfMemory,
		},
		{
			name:    "standalone OOM token",
			status:  InstanceStatusFailed,
			message: "killed by the OOM killer",
			want:    ServiceReasonOutOfMemory,
		},
		{
			name:    "image pull denied",
			status:  InstanceStatusFailed,
			message: "pull access denied for ghcr.io/x/y",
			want:    ServiceReasonImageUnreachable,
		},
		{
			name:    "missing secret",
			status:  InstanceStatusFailed,
			message: "secret app/db-creds not found",
			want:    ServiceReasonConfigMissing,
		},
		{
			name:    "liveness probe failing",
			status:  InstanceStatusFailed,
			message: "liveness probe failed: connection refused",
			want:    ServiceReasonUnhealthy,
		},
		{
			name:    "plain failed with no signal falls back to LaunchFailed",
			status:  InstanceStatusFailed,
			message: "something went wrong",
			want:    ServiceReasonLaunchFailed,
		},
		{
			name:    "exited",
			status:  InstanceStatusExited,
			message: "exited with code 0",
			want:    ServiceReasonExited,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveServiceReason(tc.status, tc.message); got != tc.want {
				t.Errorf("DeriveServiceReason(%s, %q) = %q, want %q", tc.status, tc.message, got, tc.want)
			}
		})
	}
}
