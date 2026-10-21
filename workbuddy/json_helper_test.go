package main

import (
	"testing"
	"time"
)

func TestJsonBool(t *testing.T) {
	m := map[string]any{"a": true, "b": float64(1), "c": "true", "d": "false", "e": "1", "f": float64(0)}
	if !jsonBool(m, "a") {
		t.Error("a")
	}
	if !jsonBool(m, "b") {
		t.Error("b")
	}
	if !jsonBool(m, "c") {
		t.Error("c")
	}
	if !jsonBool(m, "e") {
		t.Error("e")
	}
	if jsonBool(m, "d") {
		t.Error("d")
	}
	if jsonBool(m, "f") {
		t.Error("f")
	}
	if jsonBool(m, "missing") {
		t.Error("missing")
	}
	if !jsonBool(m, "missing", "a") {
		t.Error("fallback a")
	}
}
func TestJsonI64(t *testing.T) {
	m := map[string]any{"a": float64(42), "b": int64(7), "c": "99", "d": "abc"}
	if jsonI64(m, "a") != 42 {
		t.Error("a")
	}
	if jsonI64(m, "b") != 7 {
		t.Error("b")
	}
	if jsonI64(m, "c") != 99 {
		t.Error("c")
	}
	if jsonI64(m, "d") != 0 {
		t.Error("d")
	}
	if jsonI64(m, "missing") != 0 {
		t.Error("missing")
	}
}
func TestJsonStr(t *testing.T) {
	m := map[string]any{"a": "hello", "b": 42, "c": true}
	if jsonStr(m, "a") != "hello" {
		t.Error("a")
	}
	if jsonStr(m, "b") != "" {
		t.Error("b")
	}
	if jsonStr(m, "c") != "" {
		t.Error("c")
	}
	if jsonStr(m, "missing") != "" {
		t.Error("missing")
	}
	if jsonStr(m, "missing", "a") != "hello" {
		t.Error("fallback")
	}
}
func TestNextKeepaliveWakeTime(t *testing.T) {
	morning := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	if got := nextKeepaliveTime(morning).Hour(); got != 22 {
		t.Fatalf("want 22, got %v", got)
	}
	late := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	got := nextKeepaliveTime(late)
	if got.Hour() != 22 || got.Day() != 25 {
		t.Fatalf("want next day 22:00, got %v", got)
	}
}
