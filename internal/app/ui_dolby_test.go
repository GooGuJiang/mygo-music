//go:build windows && amd64

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"mygo-music/internal/player"
)

// The Automix info stays open as the pointer moves into it and keeps the button's click behavior.
func TestPlayerBarAutomixHover(t *testing.T) {
	a := newTestApp()
	view := ui.NewTester(a.view, 880, 580)
	settled(view)
	button, ok := view.AccessByRole(ui.RoleButton, T("Enable Automix"))
	if !ok {
		t.Fatal("missing Automix button")
	}
	r := button.Bounds
	view.Move(r.X+r.W/2, r.Y+r.H/2)
	settled(view)
	panel, ok := view.AccessByRole(ui.RoleGroup, T("Automix"))
	if !ok {
		t.Fatal("hover did not open the Automix info")
	}
	view.ClickAt(r.X+r.W/2, r.Y+r.H/2)
	if !a.settings.Automix {
		t.Fatal("the info panel intercepted the Automix button")
	}
	r = panel.Bounds
	view.Move(r.X+r.W/2, r.Y+r.H-4)
	settled(view)
	if !a.automixInfoOpen {
		t.Fatal("Automix info closed while hovering the panel")
	}
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		saveFrame(t, dir, "automix-hover.png", view.Image())
	}
	view.Move(0, 0)
	settled(view)
	if a.automixInfoOpen {
		t.Fatal("Automix info did not close when the pointer left")
	}
}

// At minimum width the metronome opens on hover and both volume changes survive a restart.
func TestPlayerBarMetronomeVolume(t *testing.T) {
	a := newTestApp()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"volume":0.5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.settings = readSettings(path)
	a.applySavedVolumes()
	view := ui.NewTester(a.view, 880, 580)
	settled(view)
	label := T("Metronome Volume")
	button, ok := view.AccessByRole(ui.RoleButton, T("Enable Metronome"))
	if !ok {
		t.Fatal("missing metronome button")
	}
	r := button.Bounds
	view.Move(r.X+r.W/2, r.Y+r.H/2)
	if _, ok := view.AccessByRole(ui.RoleSlider, label); ok {
		t.Fatal("volume slider opened while the metronome was off")
	}
	clickSettingRole(t, view, ui.RoleButton, T("Enable Metronome"))
	settled(view)
	if !a.pl.Metronome() {
		t.Fatalf("metronome did not turn on, button bounds %+v", r)
	}
	slider, ok := view.AccessByRole(ui.RoleSlider, label)
	if !ok {
		t.Fatal("hovering over the enabled metronome did not open its volume slider")
	}
	r = slider.Bounds
	y := r.Y + r.H/2
	view.Move(r.X+r.W/2, y)
	view.Press(r.X+r.W/2, y)
	view.Move(r.X+r.W, y)
	view.Release(r.X+r.W, y)
	settled(view)
	if !a.pl.Metronome() || a.pl.MetronomeVolume() != player.MetronomeMaxVolume || a.volume != 0.5 {
		t.Fatalf("drag changed metronome=%v gain=%v master=%v", a.pl.Metronome(), a.pl.MetronomeVolume(), a.volume)
	}
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		saveFrame(t, dir, "metronome-volume.png", view.Image())
	}
	view.Move(0, 0)
	settled(view)
	if _, ok := view.AccessByRole(ui.RoleSlider, label); ok {
		t.Fatal("volume slider remained open after moving away")
	}
	speaker, ok := view.AccessByRole(ui.RoleButton, T("Volume"))
	if !ok {
		t.Fatal("missing speaker button")
	}
	r = speaker.Bounds
	view.Wheel(r.X+r.W/2, r.Y+r.H/2, 0, 1)
	view.Advance(volumeSaveDelay)
	view.Frame()
	reopened := newTestApp()
	reopened.settings = readSettings(path)
	reopened.applySavedVolumes()
	if reopened.volume != 0.5-wheelVolumeStep || reopened.pl.MetronomeVolume() != player.MetronomeMaxVolume {
		t.Fatalf("reopened with master=%v and metronome gain=%v", reopened.volume, reopened.pl.MetronomeVolume())
	}
	clickSettingRole(t, view, ui.RoleButton, T("Disable Metronome"))
	settled(view)
	if a.pl.Metronome() {
		t.Fatal("metronome did not turn off")
	}
}

// Dolby picks save immediately, sliders save after input pauses, and add-ons follow preset switches.
func TestDolbySettingsAutosavesAfterSliderPause(t *testing.T) {
	a := newTestApp()
	a.settingsPage = "playback"
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	view := ui.NewTester(a.settingsView, 780, 1120)
	settled(view)

	saved := func() *settings {
		t.Helper()
		b, err := os.ReadFile(a.settings.path)
		if err != nil {
			t.Fatal(err)
		}
		s := defaultSettings()
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return &s
	}
	pick := func(preset string) {
		t.Helper()
		clickSettingRole(t, view, ui.RolePopUpButton, T("Dolby Preset"))
		settled(view)
		if err := view.Click(T(preset)); err != nil {
			t.Fatal(err)
		}
		view.Frame()
	}
	pick("Music")
	if s := saved(); s.DolbyEffect.Preset != "music" || s.dolbyProfile().IntelligentEQ <= 0 {
		t.Fatalf("music preset was not saved automatically: %+v", s.DolbyEffect)
	}
	pick("Headphone Spatial Audio")
	spatial := saved()
	if !spatial.dolbyProfile().Spatial {
		t.Fatal("headphone preset did not select spatial processing")
	}
	pick("Custom")
	clickSettingRole(t, view, ui.RoleSwitch, T("Headphone Spatial Audio"))
	view.Frame()
	before := saved()
	if !before.DolbyEffect.Custom.Spatial {
		t.Fatal("custom spatial switch did not save")
	}
	slider, ok := view.AccessByRole(ui.RoleSlider, T("Dialogue Enhancement"))
	if !ok {
		t.Fatal("missing dialogue slider")
	}
	r := slider.Bounds
	y := r.Y + r.H/2
	view.Press(r.X+r.W/4, y)
	view.Frame()
	view.Advance(dolbySliderDelay / 2)
	view.Move(r.X+r.W*3/4, y)
	view.Frame()
	view.Release(r.X+r.W*3/4, y)
	view.Frame()
	clickSettingRole(t, view, ui.RoleSlider, T("Intelligent EQ"))
	view.Frame()
	view.Advance(dolbySliderDelay / 2)
	if s := saved(); s.DolbyEffect != before.DolbyEffect {
		t.Fatalf("slider saved before the last input had settled: %+v", s.DolbyEffect)
	}
	view.Advance(dolbySliderDelay)
	view.Frame()
	got := saved()
	if got.dolbyProfile() != a.settings.dolbyProfile() || got.DolbyEffect.Preset != "custom" ||
		got.DolbyEffect.Custom.Dialogue <= 0 || got.DolbyEffect.Custom.IntelligentEQ <= 0 {
		t.Fatalf("saved effect does not include the slider edits: %+v", got.DolbyEffect)
	}
	clickSettingRole(t, view, ui.RolePopUpButton, T("Intelligent EQ Curve"))
	settled(view)
	if err := view.Click(T("Warm")); err != nil {
		t.Fatal(err)
	}
	view.Frame()
	if s := saved(); s.dolbyProfile().IntelligentEQCurve != "warm" || s.dolbyProfile().IntelligentEQ != got.dolbyProfile().IntelligentEQ {
		t.Fatalf("curve selection did not preserve the saved EQ strength: %+v", s.DolbyEffect)
	}
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		saveFrame(t, dir, "dolby-custom.png", view.Image())
	}
	clickSettingRole(t, view, ui.RoleSwitch, T("High-frequency compensation"))
	view.Frame()
	if !saved().DemucsClarity {
		t.Fatal("Demucs compensation switch did not save")
	}
	clickSettingRole(t, view, ui.RoleSwitch, T("High-frequency compensation"))
	view.Frame()
	if saved().DemucsClarity {
		t.Fatal("turning off Demucs compensation did not save")
	}
	clickSettingRole(t, view, ui.RoleSwitch, T("Advanced Dolby Controls"))
	view.Frame()
	clickSettingRole(t, view, ui.RoleSwitch, T("Volume Modeler"))
	view.Frame()
	clickSettingRole(t, view, ui.RoleSlider, T("Dolby Output Gain"))
	view.Frame()
	if s := saved(); !s.dolbyProfile().VolumeModeler || s.dolbyProfile().OutputGain != 0 {
		t.Fatalf("incorrect save before output gain slider settled: %+v", s.DolbyEffect)
	}
	view.Advance(dolbySliderDelay)
	if s := saved(); s.dolbyProfile().OutputGain >= 0 || !s.dolbyProfile().VolumeModeler {
		t.Fatalf("advanced settings did not survive saving: %+v", s.DolbyEffect)
	}
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		saveFrame(t, dir, "dolby-advanced.png", view.Image())
	}
	pick("Music")
	clickSettingRole(t, view, ui.RoleSwitch, T("Dolby Add-ons"))
	view.Frame()
	clickSettingRole(t, view, ui.RoleSwitch, T("Graphic Equalizer"))
	view.Frame()
	if !saved().dolbyProfile().GraphicEQ {
		t.Fatal("graphic EQ toggle did not save")
	}
	for _, label := range []string{T("Bass Enhancement"), T("Dialogue Ducking"), "47 Hz"} {
		node, ok := view.AccessByRole(ui.RoleSlider, label)
		if !ok {
			t.Fatalf("missing slider %q", label)
		}
		r := node.Bounds
		view.ClickAt(r.X+r.W*3/4, r.Y+r.H/2)
		view.Frame()
	}
	if addOns := saved().DolbyEffect.AddOns; addOns.BassBoost != 0 || addOns.DialogueDucking != 0 || addOns.GraphicGains[0] != 0 {
		t.Fatalf("add-on sliders saved before input settled: %+v", addOns)
	}
	view.Advance(dolbySliderDelay)
	music := saved().dolbyProfile()
	if music.BassBoost <= 0 || music.DialogueDucking <= 0 || music.GraphicGains[0] <= 0 || music.IntelligentEQ <= 0 {
		t.Fatalf("add-ons did not save with the Music preset: %+v", music)
	}
	pick("Headphone Spatial Audio")
	if spatial := saved().dolbyProfile(); !spatial.Spatial || spatial.DolbyAddOns != music.DolbyAddOns {
		t.Fatalf("preset switch lost the add-ons: %+v", spatial)
	}
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		saveFrame(t, dir, "dolby-add-ons.png", view.Image())
	}
	clickSettingRole(t, view, ui.RoleSwitch, T("Dolby Add-ons"))
	view.Frame()
	if s := saved(); s.DolbyEffect.AddOnsEnabled || s.DolbyEffect.AddOns != music.DolbyAddOns ||
		s.dolbyProfile().DolbyAddOns != (player.DolbyAddOns{}) || !s.dolbyProfile().Spatial {
		t.Fatalf("disabling add-ons did not bypass their saved settings: %+v", s.DolbyEffect)
	}
	clickSettingRole(t, view, ui.RoleSwitch, T("Dolby Add-ons"))
	view.Frame()
	if s := saved(); !s.DolbyEffect.AddOnsEnabled || s.dolbyProfile().DolbyAddOns != music.DolbyAddOns {
		t.Fatalf("re-enabling add-ons did not restore their saved values: %+v", s.DolbyEffect)
	}
}

// AI device selections persist and can be combined with Automix.
func TestDemucsDeviceSelection(t *testing.T) {
	a := newTestApp()
	a.settingsPage = "playback"
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	view := ui.NewTester(a.settingsView, 780, 1120)
	settled(view)
	for _, choice := range []struct {
		label string
		mode  player.DemucsMode
	}{{"CUDA", player.DemucsCUDA}, {"CPU", player.DemucsCPU}, {"Auto", player.DemucsAuto}, {"Off", player.DemucsOff}} {
		clickSettingRole(t, view, ui.RolePopUpButton, T("AI Spatial Audio"))
		settled(view)
		if err := view.Click(T(choice.label)); err != nil {
			t.Fatal(err)
		}
		view.Frame()
		if choice.mode == player.DemucsCUDA {
			clickSettingRole(t, view, ui.RoleSwitch, T("Automix"))
			view.Frame()
		}
		data, err := os.ReadFile(a.settings.path)
		if err != nil {
			t.Fatal(err)
		}
		s := defaultSettings()
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		if got := s.demucsMode(); got != choice.mode {
			t.Fatalf("picked %s, saved mode %q", choice.label, got)
		}
		if !s.Automix {
			t.Fatalf("Automix was not saved with AI mode %q", choice.mode)
		}
	}
}

// Each stem edit auto-saves, and reset immediately saves a neutral mix without changing other audio settings.
func TestDemucsStemMixSavesAndResets(t *testing.T) {
	a := newTestApp()
	a.settingsPage = "playback"
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	a.settings.DemucsMode = player.DemucsAuto
	a.settings.DemucsClarity = true
	a.changed()
	view := ui.NewTester(a.settingsView, 780, 1120)
	settled(view)

	saved := func() settings {
		t.Helper()
		data, err := os.ReadFile(a.settings.path)
		if err != nil {
			t.Fatal(err)
		}
		s := defaultSettings()
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	clickSettingRole(t, view, ui.RoleSwitch, T("Adaptive Placement"))
	view.Frame()
	if saved().DemucsPositioning {
		t.Fatal("turning off adaptive placement did not save")
	}
	for _, name := range []string{"AI Vocals", "AI Drums", "AI Bass", "AI Other Instruments"} {
		clickSettingRole(t, view, ui.RoleSlider, T(name))
		view.Frame()
	}
	if s := saved(); s.DemucsMix != (player.DemucsMix{}) {
		t.Fatal("stem mix saved before the slider edits settled")
	}
	view.Advance(dolbySliderDelay)
	view.Frame()
	s := saved()
	if m := s.DemucsMix; m.Vocals == 0 || m.Drums == 0 || m.Bass == 0 || m.Other == 0 {
		t.Fatalf("edits to the four stem gains did not save: %+v", m)
	}
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		saveFrame(t, dir, "demucs-stem-mix.png", view.Image())
	}
	vocals, _ := view.AccessByRole(ui.RoleSlider, T("AI Vocals"))
	r := vocals.Bounds
	view.ClickAt(r.X+r.W*3/4, r.Y+r.H/2)
	view.Frame()
	clickSettingRole(t, view, ui.RoleButton, T("Reset Stem Mix"))
	view.Frame()
	view.Advance(dolbySliderDelay)
	if reset := saved(); reset.DemucsMix != (player.DemucsMix{}) || reset.DemucsMode != s.DemucsMode || reset.DemucsClarity != s.DemucsClarity || reset.DemucsPositioning != s.DemucsPositioning {
		t.Fatalf("reset saved %+v, mode %q, clarity %t", reset.DemucsMix, reset.DemucsMode, reset.DemucsClarity)
	}
}
