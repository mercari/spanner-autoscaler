package monitoringemulator

import "testing"

func TestExtractInstanceID(t *testing.T) {
	tests := []struct {
		name    string
		filter  string
		want    string
		wantErr bool
	}{
		{
			name: "full spanner-autoscaler filter",
			filter: `metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND
		metric.label.priority = "high" AND
		resource.label.instance_id = "my-instance"`,
			want: "my-instance",
		},
		{
			name:   "spaces around equals sign",
			filter: `resource.label.instance_id  =  "spaced-instance"`,
			want:   "spaced-instance",
		},
		{
			name:    "missing instance_id",
			filter:  `metric.type = "some.metric"`,
			wantErr: true,
		},
		{
			name:    "empty filter",
			filter:  "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractInstanceID(tt.filter)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractInstanceID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("extractInstanceID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractProjectID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "valid name",
			input: "projects/my-project",
			want:  "my-project",
		},
		{
			name:    "wrong prefix",
			input:   "folders/my-project",
			wantErr: true,
		},
		{
			name:    "empty project ID",
			input:   "projects/",
			wantErr: true,
		},
		{
			name:    "no slash",
			input:   "my-project",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractProjectID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractProjectID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("extractProjectID() = %q, want %q", got, tt.want)
			}
		})
	}
}
