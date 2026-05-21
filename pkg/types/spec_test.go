package types

import (
	"testing"
)

func TestServiceSpec_Validate(t *testing.T) {
	tests := []struct {
		name    string
		spec    *ServiceSpec
		wantErr bool
	}{
		{
			name: "valid service",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
			},
			wantErr: false,
		},
		{
			name: "missing name",
			spec: &ServiceSpec{
				Image: "nginx:latest",
				Scale: 1,
			},
			wantErr: true,
		},
		{
			name: "missing image",
			spec: &ServiceSpec{
				Name:  "test-service",
				Scale: 1,
			},
			wantErr: true,
		},
		{
			name: "negative scale",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: -1,
			},
			wantErr: true,
		},
		{
			name: "zero scale (valid, defaults to 1)",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 0,
			},
			wantErr: false,
		},
		{
			name: "valid with health check",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Health: &HealthCheck{
					Liveness: &Probe{
						Type: "http",
						Path: "/health",
						Port: 8080,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid health check type",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Health: &HealthCheck{
					Liveness: &Probe{
						Type: "invalid",
						Path: "/health",
						Port: 8080,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "missing http path",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Health: &HealthCheck{
					Liveness: &Probe{
						Type: "http",
						Port: 8080,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid port",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Health: &HealthCheck{
					Liveness: &Probe{
						Type: "http",
						Path: "/health",
						Port: 0,
					},
				},
			},
			wantErr: true,
		},
		// New test cases for ports
		{
			name: "valid port configuration",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Ports: []ServicePort{
					{
						Name: "http",
						Port: 80,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "port without name",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Ports: []ServicePort{
					{
						Port: 80,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid port number",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Ports: []ServicePort{
					{
						Name: "http",
						Port: 0, // Invalid port
					},
				},
			},
			wantErr: true,
		},
		// Test for autoscale
		{
			name: "valid autoscale configuration",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Autoscale: &ServiceAutoscale{
					Enabled: true,
					Min:     1,
					Max:     5,
					Metric:  "cpu",
					Target:  "80%",
				},
			},
			wantErr: false,
		},
		{
			name: "autoscale with negative min",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Autoscale: &ServiceAutoscale{
					Enabled: true,
					Min:     -1, // Invalid min
					Max:     5,
					Metric:  "cpu",
					Target:  "80%",
				},
			},
			wantErr: true,
		},
		{
			name: "autoscale with max < min",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Autoscale: &ServiceAutoscale{
					Enabled: true,
					Min:     5,
					Max:     3, // Invalid max < min
					Metric:  "cpu",
					Target:  "80%",
				},
			},
			wantErr: true,
		},
		{
			name: "autoscale without metric",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Autoscale: &ServiceAutoscale{
					Enabled: true,
					Min:     1,
					Max:     5,
					Target:  "80%",
				},
			},
			wantErr: true,
		},
		{
			name: "autoscale without target",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Autoscale: &ServiceAutoscale{
					Enabled: true,
					Min:     1,
					Max:     5,
					Metric:  "cpu",
				},
			},
			wantErr: true,
		},
		// Test for expose
		{
			name: "valid expose configuration",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Ports: []ServicePort{
					{
						Name: "http",
						Port: 80,
					},
				},
				Expose: &ServiceExpose{
					Port: "http",
					Host: "example.com",
				},
			},
			wantErr: false,
		},
		{
			name: "expose without port",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Expose: &ServiceExpose{
					Host: "example.com",
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("ServiceSpec.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestServiceSpec_ToService(t *testing.T) {
	tests := []struct {
		name    string
		spec    *ServiceSpec
		wantErr bool
	}{
		{
			name: "valid service",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
			},
			wantErr: false,
		},
		{
			name: "zero scale stays 0, that's a way to scale to 0",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 0,
			},
			wantErr: false,
		},
		{
			name: "invalid spec",
			spec: &ServiceSpec{
				Image: "nginx:latest", // Missing name
			},
			wantErr: true,
		},
		// Tests for new fields
		{
			name: "with namespace",
			spec: &ServiceSpec{
				Name:      "test-service",
				Namespace: "test-ns",
				Image:     "nginx:latest",
				Scale:     1,
			},
			wantErr: false,
		},
		{
			name: "with ports",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Ports: []ServicePort{
					{
						Name: "http",
						Port: 80,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "with autoscale",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Autoscale: &ServiceAutoscale{
					Enabled: true,
					Min:     1,
					Max:     5,
					Metric:  "cpu",
					Target:  "80%",
				},
			},
			wantErr: false,
		},
		{
			name: "with network policy",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				NetworkPolicy: &ServiceNetworkPolicy{
					// Basic network policy configuration
					Ingress: []IngressRule{
						{
							From: []NetworkPolicyPeer{
								{
									Service: "frontend",
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "with affinity",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Affinity: &ServiceAffinity{
					Required:  []string{"region=us-east-1"},
					Preferred: []string{"zone=us-east-1a"},
				},
			},
			wantErr: false,
		},
		{
			name: "with discovery settings",
			spec: &ServiceSpec{
				Name:  "test-service",
				Image: "nginx:latest",
				Scale: 1,
				Discovery: &ServiceDiscoverySpec{
					Mode: "load-balanced",
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, err := tt.spec.ToService()
			if (err != nil) != tt.wantErr {
				t.Errorf("ServiceSpec.ToService() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			// Verify service was created correctly
			if service.Name != tt.spec.Name {
				t.Errorf("Service name = %v, want %v", service.Name, tt.spec.Name)
			}

			if service.Image != tt.spec.Image {
				t.Errorf("Service image = %v, want %v", service.Image, tt.spec.Image)
			}

			if tt.spec.Scale == 0 && service.Scale != 0 {
				t.Errorf("Service scale = %v, want 0 (zero should be preserved)", service.Scale)
			} else if tt.spec.Scale > 0 && service.Scale != tt.spec.Scale {
				t.Errorf("Service scale = %v, want %v", service.Scale, tt.spec.Scale)
			}

			// Check namespace
			if tt.spec.Namespace != "" && service.Namespace != tt.spec.Namespace {
				t.Errorf("Service namespace = %v, want %v", service.Namespace, tt.spec.Namespace)
			} else if tt.spec.Namespace == "" && service.Namespace != "default" {
				t.Errorf("Service namespace = %v, want default", service.Namespace)
			}

			// Check ports
			if len(tt.spec.Ports) > 0 && len(service.Ports) != len(tt.spec.Ports) {
				t.Errorf("Service ports count = %v, want %v", len(service.Ports), len(tt.spec.Ports))
			}

			// Check autoscale
			if tt.spec.Autoscale != nil && service.Autoscale == nil {
				t.Errorf("Service autoscale = nil, want non-nil")
			} else if tt.spec.Autoscale != nil && service.Autoscale != nil {
				if service.Autoscale.Enabled != tt.spec.Autoscale.Enabled {
					t.Errorf("Service autoscale.Enabled = %v, want %v", service.Autoscale.Enabled, tt.spec.Autoscale.Enabled)
				}
				if service.Autoscale.Min != tt.spec.Autoscale.Min {
					t.Errorf("Service autoscale.Min = %v, want %v", service.Autoscale.Min, tt.spec.Autoscale.Min)
				}
				if service.Autoscale.Max != tt.spec.Autoscale.Max {
					t.Errorf("Service autoscale.Max = %v, want %v", service.Autoscale.Max, tt.spec.Autoscale.Max)
				}
			}

			// Check network policy
			if tt.spec.NetworkPolicy != nil && service.NetworkPolicy == nil {
				t.Errorf("Service networkPolicy = nil, want non-nil")
			}

			// Check affinity
			if tt.spec.Affinity != nil && service.Affinity == nil {
				t.Errorf("Service affinity = nil, want non-nil")
			} else if tt.spec.Affinity != nil && service.Affinity != nil {
				if len(service.Affinity.Required) != len(tt.spec.Affinity.Required) {
					t.Errorf("Service affinity.Required count = %v, want %v", len(service.Affinity.Required), len(tt.spec.Affinity.Required))
				}
			}

			// Check discovery
			if tt.spec.Discovery != nil && service.Discovery == nil {
				t.Errorf("Service discovery = nil, want non-nil")
			} else if tt.spec.Discovery != nil && service.Discovery != nil {
				if service.Discovery.Mode != tt.spec.Discovery.Mode {
					t.Errorf("Service discovery.Mode = %v, want %v", service.Discovery.Mode, tt.spec.Discovery.Mode)
				}
			}

			if service.ID == "" {
				t.Error("Service ID was not generated")
			}

			if service.Status != ServiceStatusPending {
				t.Errorf("Service status = %v, want %v", service.Status, ServiceStatusPending)
			}
		})
	}
}
