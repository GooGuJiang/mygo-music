package app

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"

	"mygo-music/internal/player"
)

const (
	dolbySupported   = runtime.GOOS == "windows" && runtime.GOARCH == "amd64"
	dolbySliderDelay = 200 * time.Millisecond
)

type dolbySelection struct {
	Preset        string              `json:"preset"`
	Custom        player.DolbyProfile `json:"custom"`
	AddOns        player.DolbyAddOns  `json:"addOns,omitzero"`
	AddOnsEnabled bool                `json:"addOnsEnabled,omitempty"`
}

var dolbyPresets = [...]struct {
	id, label string
	profile   player.DolbyProfile
}{
	{"balanced", N("Balanced"), player.DolbyProfile{Leveler: 2, Target: -24}},
	{"dynamic", N("Dynamic"), player.DolbyProfile{Leveler: 4, Target: -24, Dialogue: 5, IntelligentEQ: 10}},
	{"music", N("Music"), player.DolbyProfile{IntelligentEQ: 8}},
	{"atmos", N("Headphone Spatial Audio"), player.DolbyProfile{Spatial: true}},
	{"movie", N("Movie"), player.DolbyProfile{Leveler: 4, Target: -26, Dialogue: 8}},
	{"game", N("Game"), player.DolbyProfile{Leveler: 1, Target: -28, Dialogue: 3}},
	{"voice", N("Voice"), player.DolbyProfile{Dialogue: 12}},
	{"night", N("Night"), player.DolbyProfile{Leveler: 6, Target: -32}},
	{"custom", N("Custom"), player.DolbyProfile{}},
}

func dolbyPresetIndex(id string) int {
	for i, p := range dolbyPresets {
		if p.id == id {
			return i
		}
	}
	return 0
}

func (s *settings) dolbyProfile() player.DolbyProfile {
	if !s.Dolby {
		return player.DolbyProfile{}
	}
	effect := s.DolbyEffect
	preset := dolbyPresets[dolbyPresetIndex(effect.Preset)]
	profile := preset.profile
	if effect.Preset == "custom" {
		profile = effect.Custom
	}
	if effect.AddOnsEnabled {
		profile.DolbyAddOns = effect.AddOns
	}
	profile.Preset = preset.id
	return profile
}

func (s *settings) demucsMode() player.DemucsMode {
	if s.Dolby {
		switch s.DemucsMode {
		case player.DemucsAuto, player.DemucsCUDA, player.DemucsCPU:
			return s.DemucsMode
		}
	}
	return player.DemucsOff
}

func (a *app) dolbyButton(c *ui.Context) {
	active, tried := a.pl.DolbyStatus()
	p := a.pal
	color := p.muted
	if active {
		color = p.accent
	}
	label := T("Dolby Processing")
	b := iconButton(c.Key("dolby-button"), iconDolby, 16, color, label)
	if b.Clicked() {
		a.dolbyOpen = !a.dolbyOpen
	}
	ui.PopoverBase(c.Key("dolby-popover"), b, &a.dolbyOpen, func(panel ui.Element) {
		pop := popoverColors(p.dark)
		panel.AttachTo(b, ui.AnchorTopEnd, ui.AnchorBottomEnd).Margin(0, 0, 9, 0).
			Width(288).Padding(14).Gap(8).Radius(12).Background(pop.fill).Border(0.5, pop.edge).
			Shadow(0, 8, 24, -4, ui.RGBA(0, 0, 0, 0.2)).Label(label)
		state := a.pl.DolbyProcessing()
		demucs := a.pl.DemucsStatus()
		ui.Row(c).AlignItems(ui.Center).Justify(ui.SpaceBetween).Gap(10).Children(func() {
			ui.Text(c, T("Dolby Audio")).FontSize(15).FontWeight(600)
			if state.Active {
				ui.Text(c, T(dolbyPresets[dolbyPresetIndex(state.Profile.Preset)].label)).FontSize(12).TextColor(p.muted)
			}
		})
		status := T("Preparing Dolby processing…")
		switch {
		case demucs.Requested && (a.pl.Current() == nil || a.pl.Loading()):
			status = demucsPreparation(demucs, false)
		case a.pl.Current() == nil:
			status = T("No track selected")
		case state.Pending && !a.pl.Playing():
			status = T("Dolby changes will apply when playback resumes")
		case state.Pending:
			status = T("Applying Dolby settings…")
		case !a.settings.Dolby:
			status = T("Dolby Audio is off")
		case state.Active && !a.pl.Playing():
			status = T("Playback paused")
		case state.Active:
			status = ""
			if demucs.Requested && !demucs.Active {
				status = demucsPreparation(demucs, true)
			}
		case tried || a.pl.Err() != "" || a.pl.Ready() && state.InputChannels == 0:
			status = T("Dolby Audio is unavailable for this track.")
		case state.InputChannels > 0:
			status = T("No Dolby effects enabled")
		case !a.pl.Playing() && !a.pl.Loading():
			status = T("Playback paused")
		}
		if status != "" {
			ui.Text(c, status).FontSize(12).TextColor(p.muted)
		}
		a.dolbyProcessingDetails(c, state, demucs)
	})
}

func demucsPreparation(state player.DemucsStatus, playing bool) string {
	switch {
	case state.Failed:
		return T("Demucs unavailable")
	case state.Device == player.DemucsOff:
		return T("Warming Demucs…")
	case playing:
		return Tf("Demucs %s · waiting for audio", demucsDevice(state.Device))
	default:
		return Tf("Demucs %s ready", demucsDevice(state.Device))
	}
}

func demucsDevice(mode player.DemucsMode) string {
	if mode == player.DemucsCUDA {
		return "CUDA"
	}
	return "CPU"
}

func (a *app) dolbyProcessingDetails(c *ui.Context, state player.DolbyProcessing, demucs player.DemucsStatus) {
	if state.InputChannels == 0 || !state.Active && !demucs.Active {
		return
	}
	p := a.pal
	profile := state.Profile
	input := "PCM"
	switch {
	case demucs.Active:
		input = "Demucs " + demucsDevice(demucs.Device)
	case a.pl.DolbyAtmos():
		input = "Dolby Atmos"
	}
	route := input
	if state.Active {
		route += " → Dolby APO"
	}
	route += " → PCM"
	ui.Column(c.Key("dolby-signal")).Gap(3).Padding(9, 10).Radius(8).Background(p.hover).Children(func() {
		ui.Text(c, route).FontSize(13).FontWeight(500)
		ui.Row(c).AlignItems(ui.Center).Justify(ui.SpaceBetween).Gap(6).Children(func() {
			ui.Text(c, Tf("%d Hz · %d → 2 channels", state.SampleRate, state.InputChannels)).
				FontSize(12).FontFeatures("tnum").TextColor(p.muted)
			if demucs.Active && demucs.Speed > 0 {
				ui.Text(c, Tf("Separation %.1f×", demucs.Speed)).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
			}
		})
	})
	if !state.Active {
		return
	}
	row := func(label, value string) {
		ui.Row(c).AlignItems(ui.Center).Justify(ui.SpaceBetween).Gap(10).Children(func() {
			ui.Text(c, label).FontSize(12).TextColor(p.muted)
			ui.Text(c, value).FontSize(12).FontWeight(500).FontFeatures("tnum").TextAlign(ui.End)
		})
	}
	if !a.settings.Dolby || !a.settings.DolbyEffect.AddOnsEnabled {
		return
	}
	if profile.BassBoost > 0 {
		row(T("Bass Enhancement"), fmt.Sprintf("+%g dB", profile.BassBoost))
	}
	if profile.GraphicEQ {
		bands := 0
		for _, gain := range profile.GraphicGains {
			if gain != 0 {
				bands++
			}
		}
		if bands > 0 {
			row(T("Graphic Equalizer"), Tf("%d of 20 bands adjusted", bands))
		}
	}
	if profile.DialogueDucking > 0 {
		row(T("Dialogue Ducking"), fmt.Sprintf("%g / 16", profile.DialogueDucking))
	}
}

func (a *app) dolbySettings(c *ui.Context, row settingsRow) {
	s := &a.settings
	effect := &s.DolbyEffect
	row(T("Dolby Audio"), "", func() {
		if ui.Switch(c.Key("dolby"), &s.Dolby).Label(T("Dolby Audio")).Changed() {
			a.pl.PrepareDemucs()
			a.saveDolby()
		}
	})
	presetHelp := ""
	if effect.Preset == "atmos" {
		presetHelp = T("Render stereo, surround, and supported Atmos tracks for headphones.")
	}
	row(T("Dolby Preset"), presetHelp, func() {
		labels := make([]string, len(dolbyPresets))
		for i, p := range dolbyPresets {
			labels[i] = p.label
		}
		if i, changed := choice(c, "dolby-preset", T("Dolby Preset"), labels, dolbyPresetIndex(effect.Preset)); changed {
			effect.Preset = dolbyPresets[i].id
			a.saveDolby()
		}
	})
	if effect.Preset == "custom" {
		profile := &effect.Custom
		row(T("Headphone Spatial Audio"), T("Render stereo, surround, and supported Atmos tracks for headphones."), func() {
			if ui.Switch(c.Key("dolby-spatial"), &profile.Spatial).Label(T("Headphone Spatial Audio")).Changed() {
				a.saveDolby()
			}
		})
		a.dolbySlider(c, row, "dolby-leveler", T("Loudness Leveling"), T("Reduce the difference between quiet and loud parts."), &profile.Leveler, 0, 10, "", false)
		a.dolbySlider(c, row, "dolby-target", T("Target Loudness"), "", &profile.Target, -34, -18, " dB", profile.Leveler == 0)
		a.dolbySlider(c, row, "dolby-dialogue", T("Dialogue Enhancement"), T("Make voices easier to hear."), &profile.Dialogue, 0, 16, "", false)
		a.dolbySlider(c, row, "dolby-ieq", T("Intelligent EQ"), "", &profile.IntelligentEQ, 0, 16, "", false)
		row(T("Intelligent EQ Curve"), "", func() {
			curves := []string{"balanced", "detailed", "warm"}
			at := max(0, slices.Index(curves, profile.IntelligentEQCurve))
			if i, changed := choice(c, "dolby-ieq-curve", T("Intelligent EQ Curve"), []string{N("Balanced"), N("Detailed"), N("Warm")}, at); changed {
				profile.IntelligentEQCurve = curves[i]
				a.saveDolby()
			}
		})
		row(T("Advanced Dolby Controls"), "", func() {
			ui.Switch(c.Key("dolby-advanced"), &a.dolbyAdvanced).Label(T("Advanced Dolby Controls"))
		})
		if a.dolbyAdvanced {
			a.dolbyAdvancedSettings(c, row, profile)
		}
	}
	row(T("AI Spatial Audio"), T("Applies from the next track."), func() {
		modes := []player.DemucsMode{player.DemucsOff, player.DemucsAuto, player.DemucsCUDA, player.DemucsCPU}
		at := max(0, slices.Index(modes, s.DemucsMode))
		if i, changed := choice(c, "demucs-mode", T("AI Spatial Audio"), []string{N("Off"), N("Auto"), "CUDA", "CPU"}, at); changed {
			s.DemucsMode = modes[i]
			a.pl.PrepareDemucs()
			a.changed()
		}
	})
	cudaHelp := T("Import the Windows x64 CUDA ZIP, then select Auto or CUDA. Applies from the next track.")
	if a.cudaImportMessage != "" {
		cudaHelp = a.cudaImportMessage
	}
	row(T("CUDA Package"), cudaHelp, func() {
		label := T("Import CUDA Package")
		if a.cudaImporting {
			label = T("Importing CUDA package…")
		}
		ui.Button(c.Key("import-cuda"), label).Disabled(a.cudaImporting).OnClick(a.startCUDAImport)
	})
	row(T("Automix"), T("Crossfade queued tracks. Demucs adds beat and key matching in stereo."), func() {
		if ui.Switch(c.Key("automix"), &s.Automix).Label(T("Automix")).Changed() {
			a.changed()
		}
	})
	row(T("High-frequency compensation"), "", func() {
		if ui.Switch(c.Key("demucs-clarity"), &s.DemucsClarity).Label(T("High-frequency compensation")).Disabled(!s.Dolby).Changed() {
			a.saveDolby()
		}
	})
	demucsMixEnabled := s.Dolby && (s.demucsMode() != player.DemucsOff || a.pl.DemucsStatus().Requested)
	hasStemEdits := s.DemucsMix != (player.DemucsMix{})
	if demucsMixEnabled || hasStemEdits {
		row(T("Adaptive Placement"), "", func() {
			if ui.Switch(c.Key("demucs-positioning"), &s.DemucsPositioning).Label(T("Adaptive Placement")).Disabled(!demucsMixEnabled).Changed() {
				a.saveDolby()
			}
		})
		a.dolbySlider(c, row, "demucs-vocals", T("AI Vocals"), "", &s.DemucsMix.Vocals, player.DemucsMinGain, player.DemucsMaxGain, " dB", !demucsMixEnabled)
		a.dolbySlider(c, row, "demucs-drums", T("AI Drums"), "", &s.DemucsMix.Drums, player.DemucsMinGain, player.DemucsMaxGain, " dB", !demucsMixEnabled)
		a.dolbySlider(c, row, "demucs-bass", T("AI Bass"), "", &s.DemucsMix.Bass, player.DemucsMinGain, player.DemucsMaxGain, " dB", !demucsMixEnabled)
		a.dolbySlider(c, row, "demucs-other", T("AI Other Instruments"), "", &s.DemucsMix.Other, player.DemucsMinGain, player.DemucsMaxGain, " dB", !demucsMixEnabled)
		row(T("Stem Mix"), "", func() {
			if ui.Button(c.Key("demucs-mix-reset"), T("Reset Stem Mix")).Disabled(!hasStemEdits).Clicked() {
				s.DemucsMix = player.DemucsMix{}
				a.saveDolby()
			}
		})
	}
	row(T("Dolby Add-ons"), "", func() {
		if ui.Switch(c.Key("dolby-add-ons"), &effect.AddOnsEnabled).Label(T("Dolby Add-ons")).Changed() {
			a.saveDolby()
		}
	})
	if effect.AddOnsEnabled {
		a.dolbyAddOnSettings(c, row, &effect.AddOns)
	}
}

func (a *app) dolbyAddOnSettings(c *ui.Context, row settingsRow, addOns *player.DolbyAddOns) {
	a.dolbySlider(c, row, "dolby-bass", T("Bass Enhancement"), "", &addOns.BassBoost, 0, 12, " dB", false)
	a.dolbySlider(c, row, "dolby-ducking", T("Dialogue Ducking"), T("Lower the background when dialogue is detected. Set to 0 to turn off."), &addOns.DialogueDucking, 0, 16, "", false)
	row(T("Graphic Equalizer"), "", func() {
		if ui.Switch(c.Key("dolby-graphic-eq"), &addOns.GraphicEQ).Label(T("Graphic Equalizer")).Changed() {
			a.saveDolby()
		}
	})
	if addOns.GraphicEQ {
		for i, frequency := range player.DolbyEQFrequencies(48000) {
			hz := strconv.Itoa(int(frequency))
			a.dolbySlider(c, row, "dolby-graphic-"+hz, hz+" Hz", "", &addOns.GraphicGains[i], -12, 12, " dB", false)
		}
	}
}

func (a *app) dolbyAdvancedSettings(c *ui.Context, row settingsRow, profile *player.DolbyProfile) {
	a.dolbySlider(c, row, "dolby-input-target", T("Input Loudness Target"), "", &profile.InputTarget, -40, -12, " dB", profile.Leveler == 0)
	a.dolbySlider(c, row, "dolby-gain", T("Dolby Output Gain"), "", &profile.OutputGain, -24, 6, " dB", false)
	for _, setting := range []struct {
		key, label string
		value      *bool
		disabled   bool
	}{
		{"dolby-ieq-steering", T("Content-Adaptive EQ"), &profile.IntelligentEQSteering, profile.IntelligentEQ == 0},
		{"dolby-volume-modeler", T("Volume Modeler"), &profile.VolumeModeler, false},
		{"dolby-bypass-surround", T("Bypass Surround Decoder"), &profile.DisableSurroundDecoder, !profile.Spatial},
	} {
		row(setting.label, "", func() {
			if ui.Switch(c.Key(setting.key), setting.value).Label(setting.label).Disabled(setting.disabled).Changed() {
				a.saveDolby()
			}
		})
	}
	a.dolbySlider(c, row, "dolby-volume-calibration", T("Volume Modeler Calibration"), "", &profile.VolumeCalibration, -20, 20, " dB", !profile.VolumeModeler)
}

func (a *app) saveDolby() {
	a.dolbySaveAt = time.Time{}
	a.pl.ApplyDolby()
	a.changed()
}

func (a *app) dolbySlider(c *ui.Context, row settingsRow, key, label, help string, value *float64, lo, hi float64, suffix string, disabled bool) {
	row(label, help, func() {
		ui.Row(c.Key(key)).AlignItems(ui.Center).Gap(8).Children(func() {
			if ui.Slider(c, value, lo, hi).Step(1).Width(124).Label(label).Disabled(disabled).Changed() {
				a.dolbySaveAt = c.Now().Add(dolbySliderDelay)
			}
			ui.Text(c, strconv.Itoa(int(*value))+suffix).FontSize(12).Width(40)
		})
	})
}
