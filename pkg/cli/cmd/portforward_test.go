package cmd

import (
	"reflect"
	"testing"
)

func TestParsePortMappings(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []portMapping
		wantErr bool
	}{
		{name: "single bare", in: []string{"27017"}, want: []portMapping{{local: 27017, remote: 27017}}},
		{name: "single mapped", in: []string{"27018:27017"}, want: []portMapping{{local: 27018, remote: 27017}}},
		{name: "multiple", in: []string{"9001", "9002:9000"}, want: []portMapping{{local: 9001, remote: 9001}, {local: 9002, remote: 9000}}},
		{name: "zero remote", in: []string{"1:0"}, wantErr: true},
		{name: "zero local", in: []string{"0:80"}, wantErr: true},
		{name: "non-numeric", in: []string{"abc"}, wantErr: true},
		{name: "out of range", in: []string{"99999"}, wantErr: true},
		{name: "empty", in: []string{""}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePortMappings(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
