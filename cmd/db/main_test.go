package main

import (
	"reflect"
	"testing"
)

func TestParseShards(t *testing.T) {
	got, err := parseShards(" shard-1 = a:1, b:1 ; shard-2=c:1 ")
	if err != nil {
		t.Fatalf("parseShards: %v", err)
	}
	want := map[string][]string{
		"shard-1": {"a:1", "b:1"},
		"shard-2": {"c:1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseShardsErrors(t *testing.T) {
	cases := []string{
		"",
		"shard-1",
		"shard-1=",
		"shard-1=a;shard-1=b",
		"=a",
	}
	for _, in := range cases {
		if _, err := parseShards(in); err == nil {
			t.Errorf("parseShards(%q) expected an error", in)
		}
	}
}

func TestValidateCommon(t *testing.T) {
	tests := []struct {
		name                  string
		user, pass, cert, key string
		rate                  float64
		burst                 int
		wantErr               bool
	}{
		{name: "both empty", wantErr: false},
		{name: "user without pass", user: "u", wantErr: true},
		{name: "pass without user", pass: "p", wantErr: true},
		{name: "both auth", user: "u", pass: "p"},
		{name: "cert without key", cert: "c", wantErr: true},
		{name: "key without cert", key: "k", wantErr: true},
		{name: "negative rate", rate: -1, wantErr: true},
		{name: "zero burst", rate: 1, burst: 0, wantErr: true},
		{name: "valid rate", rate: 10, burst: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCommon(tt.user, tt.pass, tt.cert, tt.key, tt.rate, tt.burst)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
