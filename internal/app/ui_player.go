package app

import (
	"log"
	"math"
	"strconv"
	"time"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"

	"mygo-music/internal/player"
)

// scrubState is a progress bar's own: the value dragged, while it is, and
// the levels of the music its smoke follows.
type scrubState struct {
	v float64
	// held is set while the pointer pressed on the bar is down, wherever
	// it has gone since: Pressed is false off the bar, which is thin.
	held   bool
	active bool // dragged since the last frame, to seek once let go
	level  barState
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// scrubber shows how far the track is, and seeks where it is dragged or
// clicked: the bar thickens under the pointer, and its knob shows. With a
// smoke color, the bar is a comet while music plays: what has played is a
// streak brightening to the playhead, which glows and trails smoke that
// rises and fades, thicker as the music is louder.
func (a *app) scrubber(c *ui.Context, st *scrubState, track, fill ui.Color, height float32, smoke ui.Color) ui.Element {
	pl := a.pl
	dur := pl.Duration().Seconds()
	if !st.active && !st.held {
		st.v = pl.Position().Seconds()
	}
	s := ui.SliderBase(c, &st.v, 0, max(dur, 1)).Height(height).FocusRing(false).Label(T("Progress")).Cursor(ui.CursorPointer)
	point, estimated, hasPoint := pl.AutomixPoint()
	if hasPoint {
		s.Description(automixPointLabel(point, estimated))
	}
	marker := a.pal.accent
	// The press and its release come to the bar wherever the pointer is,
	// and go on to the slider, which follows the pointer.
	s.HandleInput(func(ev ui.InputEvent) bool {
		switch ev.Kind {
		case ui.InputPointerDown:
			st.held = ev.Button == 0
		case ui.InputPointerUp:
			st.held = false
		}
		return false
	})
	pressed := st.held
	switch {
	case pressed:
		st.active = true
	case st.active:
		// Let go: there, but not right at the end, which would end the
		// song as it begins to play.
		st.active = false
		pl.Seek(secs(min(st.v, max(0, dur-1))))
	case s.Changed():
		pl.Seek(secs(min(st.v, max(0, dur-1))))
	}
	hot := s.Hovered() || pressed
	thick := float32(4)
	if hot {
		thick = 6
	}
	thick = s.AnimateTo("thick", thick, ui.Snappy)
	knob := float32(0)
	if hot {
		knob = 1
	}
	knob = s.AnimateTo("knob", knob, ui.Snappy)
	// The smoke settles away as the music pauses, and while dragging.
	smoking := float32(0)
	if smoke.A > 0 && a.settings.Smoke && pl.Playing() && !pl.Loading() && !st.active {
		smoking = 1
	}
	smoking = s.AnimateTo("smoke", smoking, ui.Animation{Duration: 600 * time.Millisecond})
	s.Draw(func(p *ui.Painter, r ui.Rect) {
		at := st.v
		if !st.active {
			at = pl.Position().Seconds()
		}
		frac := float32(0)
		if dur > 0 {
			frac = float32(min(1, max(0, at/dur)))
		}
		y := r.Y + (r.H-thick)/2
		head, cy := r.X+r.W*frac, r.Y+r.H/2
		p.Fill(ui.Rect{X: r.X, Y: y, W: r.W, H: thick}, track, thick/2)
		// What has downloaded, a shade lighter.
		if got, ok := pl.Downloaded(); ok && float32(got) < 1 {
			p.Fill(ui.Rect{X: r.X, Y: y, W: r.W * float32(got), H: thick}, track, thick/2)
		}
		played := ui.Rect{X: r.X, Y: y, W: max(thick, head-r.X), H: thick}
		if smoking > 0.01 {
			// A streak of light, brightest at the head.
			p.FillGradient(played, ui.LinearGradient{Angle: 90, Oklab: true,
				From: fill.Mix(smoke, smoking).Alpha(0.3), To: fill.Mix(smoke, 0.8*smoking)}, thick/2)
			lv, live := pl.Levels()
			st.level.step(p.Now(), lv, live)
			bass := st.level.group(0, 6)
			drawSmoke(p, head, cy, p.Now(), bass, smoke, smoking)
			// The glow of the head, swelling with the bass, and its core.
			glow := 9 + 8*bass
			p.Shadow(ui.Rect{X: head, Y: cy}, 0, 0, 0, glow, 2.5+2.5*bass, smoke.Alpha(0.95*smoking))
			core := thick * (1 + 0.25*bass)
			p.Fill(ui.Rect{X: head - core/2, Y: cy - core/2, W: core, H: core}, white.Mix(smoke, 0.15).Alpha(smoking), core/2)
		} else {
			p.Fill(played, fill, thick/2)
		}
		if hasPoint && dur > 0 {
			x := r.X + r.W*float32(min(1, max(0, point.Seconds()/dur)))
			if estimated {
				p.Stroke(ui.Rect{X: x - 2, Y: cy - 5, W: 4, H: 10}, marker, 2, 1.5)
			} else {
				p.Fill(ui.Rect{X: x - 1.5, Y: cy - 5, W: 3, H: 10}, marker, 1.5)
			}
		}
		if knob > 0.01 {
			k := 12 * knob
			p.Shadow(ui.Rect{X: head - k/2, Y: r.Y + (r.H-k)/2, W: k, H: k}, k/2, 0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.3))
			p.Fill(ui.Rect{X: head - k/2, Y: r.Y + (r.H-k)/2, W: k, H: k}, fill, k/2)
		}
		switch {
		case smoking > 0.01 && (pl.Playing() || smoking < 1):
			// The smoke drifts slowly, under a point a frame at 30 frames
			// a second, which show it smooth for a quarter of what a 120 Hz
			// display would draw.
			p.After(time.Second / 30)
		case pl.Playing() && !st.active && dur > 0:
			// Again when the fill has moved a pixel of the screen, which
			// takes a good part of a second, rather than every frame.
			perPixel := dur / float64(r.W*p.Scale())
			p.After(max(16*time.Millisecond, secs(perPixel)))
		}
	})
	return s
}

// Smoke from the playhead: wisps shed one after another, each drifting
// back along the bar and rising, growing and fading over its life. Where a
// wisp is follows from the time alone, so nothing is kept between frames.
const (
	smokeLife  = 1.4 // seconds
	smokeWisps = 36  // alive at once, at most
)

// drawSmoke draws the smoke of a playhead at (x, y) at now, thicker as
// loud (0 to 1) is higher, at opacity shown.
func drawSmoke(p *ui.Painter, x, y float32, now time.Time, loud float32, color ui.Color, shown float32) {
	t := float64(now.UnixMilli()) / 1000
	interval := smokeLife / smokeWisps
	first := math.Floor(t / interval)
	for j := range smokeWisps {
		k := first - float64(j)
		age := float32(t - k*interval)
		if age < 0 || age > smokeLife {
			continue
		}
		// A wisp of its own: how fast it drifts and rises, how it curls,
		// how large it grows. Quiet music sheds fewer of them.
		r := func(o float64) float32 { return lineRandom(k, o) }
		if r(5) > 0.6+0.4*loud {
			continue
		}
		f := age / smokeLife
		wx := x - age*(12+22*r(1))
		wy := y - age*(12+18*r(2)) + float32(math.Sin(float64(age)*(2.5+2*float64(r(3)))+float64(r(4))*2*math.Pi))*6*f
		size := 3 + f*(12+10*r(4))
		alpha := 0.5 * float32(math.Pow(float64(1-f), 1.4)) * shown * (0.75 + 0.5*loud)
		p.Shadow(ui.Rect{X: wx, Y: wy}, 0, 0, 0, size, size*0.45, color.Alpha(alpha))
	}
}

// tickTimes redraws the view as the next second of the track begins, for
// the times shown.
func (a *app) tickTimes(c *ui.Context) {
	if a.pl.Playing() && a.pl.Ready() {
		pos := a.pl.Position()
		c.After(time.Second - pos%time.Second + 10*time.Millisecond)
	}
}

// volumeButton is the speaker, which opens the volume in a popover above
// it, as an AppKit popover: a native slider standing up, the volume in
// percent over it. The wheel over the speaker turns it up and down
// without opening it.
func (a *app) volumeButton(c *ui.Context, color ui.Color) {
	b := iconButton(c.Key("volume-button"), volumeIcon(a.volume), 16, color, T("Volume"))
	if b.Clicked() {
		a.volumeOpen = !a.volumeOpen
	}
	b.HandleInput(func(ev ui.InputEvent) bool { return a.scrollVolume(ev) })
	pop := popoverColors(a.pal.dark)
	ui.PopoverBase(c.Key("volume-popover"), b, &a.volumeOpen, func(panel ui.Element) {
		const arrow = 7
		panel.AttachTo(b, ui.AnchorTop, ui.AnchorBottom).Margin(0, 0, arrow+3, 0).Width(52).Padding(11, 0, 12, 0).
			Radius(12).Background(pop.fill).Border(0.5, pop.edge).Shadow(0, 8, 24, -4, ui.RGBA(0, 0, 0, 0.2)).
			AlignItems(ui.Center).Gap(8).Label(T("Volume"))
		panel.Draw(func(p *ui.Painter, r ui.Rect) {
			// The arrow pointing at the speaker, its sides edged as the
			// panel is, over the panel's border.
			cx, foot := r.X+r.W/2, r.Y+r.H
			tip := (&ui.Path{}).MoveTo(cx-arrow-1, foot-0.75).LineTo(cx-1.2, foot+arrow-1.2).
				QuadTo(cx, foot+arrow, cx+1.2, foot+arrow-1.2).LineTo(cx+arrow+1, foot-0.75).Close()
			p.FillPath(tip, pop.fill)
			edge := (&ui.Path{}).MoveTo(cx-arrow-1, foot).LineTo(cx-1.2, foot+arrow-1.2).
				QuadTo(cx, foot+arrow, cx+1.2, foot+arrow-1.2).LineTo(cx+arrow+1, foot)
			p.StrokePath(edge, 0.5, pop.edge)
		})
		ui.Textf(c, "%d%%", int(a.volume*100+0.5)).FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(a.pal.muted)
		a.nativeVolumeSlider(c, pop)
	})
}

// popover holds the colors of a popover as AppKit draws them.
type popover struct {
	fill, edge, track, knob ui.Color
}

func popoverColors(dark bool) popover {
	if dark {
		return popover{fill: ui.Hex("#2b2b2e"), edge: ui.RGBA(255, 255, 255, 0.14), track: ui.RGBA(255, 255, 255, 0.16), knob: ui.Hex("#ececec")}
	}
	return popover{fill: ui.Hex("#f6f6f6"), edge: ui.RGBA(0, 0, 0, 0.12), track: ui.RGBA(0, 0, 0, 0.12), knob: ui.Hex("#ffffff")}
}

// nativeVolumeSlider is a vertical slider as AppKit draws one: a thin
// track, filled with the accent from the foot to a round knob.
func (a *app) nativeVolumeSlider(c *ui.Context, pop popover) {
	const knob, track = 20, 4
	s := ui.SliderBase(c.Key("volume"), &a.volume, 0, 1).Vertical().Size(knob+8, 124).PaddingY(knob / 2).
		Step(0.01).AutoFocus().FocusRing(false).Label(T("Volume"))
	if s.Changed() {
		a.volumeChanged()
	}
	s.HandleInput(func(ev ui.InputEvent) bool { return a.scrollVolume(ev) })
	pressed := s.Pressed()
	v := float32(a.volume)
	accent := a.pal.accent
	s.Draw(func(p *ui.Painter, r ui.Rect) {
		top, foot := r.Y+knob/2, r.Y+r.H-knob/2
		x := r.X + (r.W-track)/2
		y := foot - (foot-top)*v
		p.Fill(ui.Rect{X: x, Y: top - track/2, W: track, H: foot - top + track}, pop.track, track/2)
		p.Fill(ui.Rect{X: x, Y: y, W: track, H: foot - y + track/2}, accent, track/2)
		k := ui.Rect{X: r.X + (r.W-knob)/2, Y: y - knob/2, W: knob, H: knob}
		p.Shadow(k, knob/2, 0, 0.5, 1.5, 0, ui.RGBA(0, 0, 0, 0.3))
		p.Shadow(k, knob/2, 0, 1, 4, 0, ui.RGBA(0, 0, 0, 0.12))
		fill := pop.knob
		if pressed {
			fill = fill.Mix(ui.RGB(0, 0, 0), 0.06)
		}
		p.Fill(k, fill, knob/2)
		p.Stroke(k, ui.RGBA(0, 0, 0, 0.08), knob/2, 0.5)
	})
}

// wheelVolumeStep is how much one mouse-wheel event turns the volume.
const wheelVolumeStep = 0.03

const volumeSaveDelay = 200 * time.Millisecond

func (a *app) applySavedVolumes() {
	a.volume = a.settings.Volume
	a.pl.SetVolume(a.volume)
	a.pl.SetMetronomeVolume(a.settings.MetronomeVolume)
}

func (a *app) volumeChanged() {
	a.pl.SetVolume(a.volume)
	if a.settings.Volume != a.volume {
		a.settings.Volume = a.volume
		a.volumesDirty = true
	}
}

func (a *app) saveVolumes() {
	a.volumesDirty = false
	a.volumeSaveAt = time.Time{}
	a.settings.Volume = a.volume
	if err := a.settings.save(); err != nil {
		log.Printf("saving volume settings: %v", err)
	}
}

// scrollVolume turns the volume up as the wheel or the fingers scroll up.
// Fingers on a touchpad turn it as far as they scroll; a mouse wheel turns
// it a fixed step per event, as how far a notch scrolls depends on the
// system's lines-per-notch setting, so a notch was a third of the volume
// on some machines.
func (a *app) scrollVolume(ev ui.InputEvent) bool {
	if ev.Kind != ui.InputScroll {
		return false
	}
	a.volume = min(1, max(0, a.volume-volumeScroll(ev)))
	a.volumeChanged()
	return true
}

// volumeScroll is how much a scroll turns the volume down.
func volumeScroll(ev ui.InputEvent) float64 {
	if ev.Precise {
		return float64(ev.DY) / 400
	}
	switch {
	case ev.DY > 0:
		return wheelVolumeStep
	case ev.DY < 0:
		return -wheelVolumeStep
	}
	return 0
}

// volumeIcon is the speaker for volume v: silent, or with one or two waves.
func volumeIcon(v float64) *ui.SVG {
	switch {
	case v == 0:
		return iconMute
	case v < 0.4:
		return iconVolumeLow
	default:
		return iconVolume
	}
}

// muteButton is the speaker, which mutes and unmutes.
func (a *app) muteButton(c *ui.Context, color ui.Color) {
	icon := iconVolume
	if a.volume == 0 {
		icon = iconMute
	}
	if iconButton(c.Key("mute"), icon, 16, color, T("Mute")).Clicked() {
		if a.volume > 0 {
			a.mutedFrom, a.volume = a.volume, 0
		} else {
			a.volume = max(a.mutedFrom, 0.5)
		}
		a.volumeChanged()
	}
}

// volumeSlider is a slim slider with the speaker before it, which mutes.
func (a *app) volumeSlider(c *ui.Context, color, track ui.Color, width float32) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		a.muteButton(c, color)
		s := ui.SliderBase(c.Key("volume"), &a.volume, 0, 1).Width(width).Height(16).FocusRing(false).Label(T("Volume"))
		if s.Changed() {
			a.volumeChanged()
		}
		hot := s.Hovered() || s.Pressed()
		thick := float32(4)
		if hot {
			thick = 5
		}
		thick = s.AnimateTo("thick", thick, ui.Snappy)
		v := float32(a.volume)
		s.Draw(func(p *ui.Painter, r ui.Rect) {
			y := r.Y + (r.H-thick)/2
			p.Fill(ui.Rect{X: r.X, Y: y, W: r.W, H: thick}, track, thick/2)
			p.Fill(ui.Rect{X: r.X, Y: y, W: max(thick, r.W*v), H: thick}, color, thick/2)
		})
	})
}

// transport is the row of controls: shuffle, previous, play, next, repeat.
func (a *app) transport(c *ui.Context, color, dim, on ui.Color, size float32) {
	pl := a.pl
	ui.Row(c).Gap(size * 0.5).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
		shuffle := dim
		if pl.Shuffle() {
			shuffle = on
		}
		if iconButton(c.Key("shuffle"), iconShuffle, size*0.62, shuffle, T("Shuffle")).Clicked() {
			pl.ToggleShuffle()
		}
		if iconButton(c.Key("prev"), iconPrev, size*0.85, color, T("Previous")).Clicked() {
			pl.Prev()
		}
		if playButton(c.Key("play"), pl.Playing(), size*1.25, color).Clicked() {
			pl.TogglePlay()
		}
		if iconButton(c.Key("next"), iconNext, size*0.85, color, T("Next")).Clicked() {
			pl.Next()
		}
		repeat, ricon := dim, iconRepeat
		switch pl.Repeat() {
		case player.RepeatAll:
			repeat = on
		case player.RepeatOne:
			repeat, ricon = on, iconRepeat1
		}
		if iconButton(c.Key("repeat"), ricon, size*0.62, repeat, T("Repeat")).Clicked() {
			pl.CycleRepeat()
		}
	})
}

// playerBar runs along the foot of the content: what plays, its controls
// and its progress, and the volume.
func (a *app) playerBar(c *ui.Context) {
	p := a.pal
	pl := a.pl
	tr := pl.Current()
	a.tickTimes(c)
	// Over the foot of the page: while content is under it, its edge frosts
	// it, with a hairline along the top.
	edge := ui.Box(c.Key("bottom-edge")).Absolute().Left(0).Right(0).Bottom(0).Height(playerBarHeight).
		Material(glass.ScrollEdge{Hard: true, Bottom: true, Background: p.bg})
	edge.Opacity(edgeShown(edge, a.underBottom))
	// The controls keep the middle, between sides that share what is left:
	// narrower, the middle shrinks, and what plays keeps room
	// for its cover, some of its title and its heart.
	const barPadding, barGap, trackWidth = 20, 20, 150
	controls, middleMin, trackMin := float32(3*32+2*4), float32(300), float32(trackWidth) // lyrics, queue and speaker
	middleWidth := float32(440)
	if dolbySupported {
		controls += 3*(32+4) + 62
		middleMin, trackMin = 192, 102
		width, _ := c.Size()
		middleWidth = min(middleWidth, max(middleMin, width-sidebarWidth-2*(barPadding+barGap)-controls-trackWidth))
	}
	ui.Row(c).Absolute().Left(0).Right(0).Bottom(0).Height(playerBarHeight).PaddingX(barPadding).Gap(barGap).AlignItems(ui.Center).Children(func() {
		// What plays.
		ui.Row(c).Grow(1).Basis(0).MinWidth(trackMin).Children(func() {
			if tr == nil {
				ui.Row(c).Shrink(1).MinWidth(0).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(48, 48).Shrink(0).Radius(8).Background(p.hover).Center().Children(func() {
						ui.Icon(c, iconNote).Size(18, 18).TextColor(p.faint)
					})
					ui.Text(c, T("Not Playing")).Shrink(1).MinWidth(0).SingleLine().FontSize(13).TextColor(p.muted)
				})
				return
			}
			info := ui.ButtonBase(c.Key("info")).Gap(12).AlignItems(ui.Center).FocusRing(false).Shrink(1).MinWidth(0).Label(T("Open Now Playing"))
			hovered := info.Hovered()
			info.Children(func() {
				// Now Playing grows out of the cover, which is gone from the
				// bar while it shows.
				mini := ui.Box(c.Key("mini-cover")).Size(48, 48).Shrink(0)
				if b := mini.LayoutBounds(); b.W > 0 {
					a.npFrom = b
				}
				if a.npT > 0 && !a.npStill {
					mini.Opacity(0)
				}
				mini.Children(func() {
					a.cover(c, tr.Album.ImageURL, 120, 48, npFromRadius).Shadow(0, 2, 8, 0, p.cardShade)
					if hovered {
						ui.Box(c).Absolute().Top(0).Left(0).Right(0).Bottom(0).Radius(8).Center().
							Background(ui.RGBA(0, 0, 0, 0.35)).TextColor(ui.RGB(255, 255, 255)).Children(func() {
							ui.Icon(c, iconDown).Size(18, 18).Rotate(180)
						})
					}
				})
				ui.Column(c).Shrink(1).MinWidth(0).Gap(3).Children(func() {
					ui.Text(c, tr.Name).FontSize(13.5).FontWeight(600).SingleLine()
					// What it is waiting on shows in place of the artists.
					var sub string
					color := p.muted
					switch {
					case pl.Loading():
						sub = T("Buffering…")
					case pl.Err() != "":
						sub, color = pl.Err(), p.accent
					}
					if sub == "" {
						a.artistLinks(c, tr, p.muted, p.text).FontSize(12).SingleLine()
					} else {
						ui.Text(c, sub).FontSize(12).TextColor(color).SingleLine()
					}
				})
			})
			if info.Clicked() {
				a.nowPlaying = true
			}
			a.likeButton(c.Key("like"), tr, 16, p.muted).Margin(0, 0, 0, 4).Shrink(0)
		})

		// Controls over the progress.
		ui.Column(c).Width(middleWidth).Shrink(1).MinWidth(middleMin).Gap(0).AlignItems(ui.Stretch).Children(func() {
			a.transport(c, p.text, p.muted, p.accent, 18)
			ui.Row(c).Gap(10).AlignItems(ui.Center).Margin(-4, 0, 0, 0).Children(func() {
				ui.Text(c, clock(pl.Position())).FontSize(10.5).FontFeatures("tnum").TextColor(p.faint).Width(34).TextAlign(ui.End)
				a.scrubber(c.Key("bar-scrub"), &a.barScrub, p.press, p.text.Alpha(0.75), 14, p.accent).Grow(1)
				ui.Text(c, clock(pl.Duration())).FontSize(10.5).FontFeatures("tnum").TextColor(p.faint).Width(34)
			})
		})

		// Lyrics and volume.
		ui.Row(c).Grow(1).Basis(0).MinWidth(controls).Justify(ui.End).AlignItems(ui.Center).Gap(4).Children(func() {
			lyr := p.muted
			if a.nowPlaying {
				lyr = p.accent
			}
			label := T("Lyrics")
			if tr != nil && tr.ShowID != "" {
				label = T("About This Episode")
			}
			if iconButton(c.Key("lyrics"), iconLyrics, 16, lyr, label).Clicked() && tr != nil {
				a.nowPlaying, a.npPane = true, a.lyricsPaneChosen()
			}
			if iconButton(c.Key("queue"), iconQueue, 16, p.muted, T("Queue")).Clicked() && tr != nil {
				a.nowPlaying, a.npPane = true, paneQueue
			}
			if dolbySupported {
				a.rhythmButtons(c)
				a.dolbyButton(c)
			}
			a.volumeButton(c, p.text.Alpha(0.75))
		})
	})
}

func (a *app) rhythmButtons(c *ui.Context) {
	p := a.pal
	tempo := "— BPM"
	if bpm := a.pl.BPM(); bpm > 0 {
		tempo = strconv.Itoa(bpm) + " BPM"
	}
	ui.Text(c.Key("bpm"), tempo).FontSize(10.5).FontFeatures("tnum").TextColor(p.muted).Width(58).TextAlign(ui.End)
	mix, mixLabel := p.faint, T("Enable Automix")
	switch {
	case a.pl.AutomixActive():
		mix, mixLabel = p.accent, T("Automix in progress")
	case a.settings.Automix:
		mix, mixLabel = p.text, T("Disable Automix")
	}
	automix := iconButton(c.Key("automix"), iconAutomix, 16, mix, mixLabel)
	if automix.Clicked() {
		a.settings.Automix = !a.settings.Automix
		a.changed()
	}
	a.playerHoverPanel(c, automix, "automix-info", T("Automix"), &a.automixInfoOpen, func() bool {
		a.automixDetails(c)
		return false
	})
	click, clickLabel := p.muted, T("Enable Metronome")
	if a.pl.Metronome() {
		click, clickLabel = p.accent, T("Disable Metronome")
	}
	b := iconButton(c.Key("metronome"), iconMetronome, 16, click, clickLabel)
	if b.Clicked() {
		a.pl.SetMetronome(!a.pl.Metronome())
	}
	if !a.pl.Metronome() {
		a.metronomeVolumeOpen = false
		return
	}
	a.playerHoverPanel(c, b, "metronome-volume-popover", T("Metronome Volume"), &a.metronomeVolumeOpen, func() bool {
		var keep bool
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			volume := a.pl.MetronomeVolume()
			s := ui.Slider(c.Key("metronome-volume"), &volume, 0, player.MetronomeMaxVolume).
				Width(104).Step(0.01).Label(T("Metronome Volume"))
			if volume != a.pl.MetronomeVolume() {
				a.pl.SetMetronomeVolume(volume)
				a.settings.MetronomeVolume = volume
				a.volumesDirty = true
			}
			ui.Textf(c, "%d%%", int(volume*100+0.5)).Width(30).TextAlign(ui.End).
				FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(p.muted)
			_, pressed := s.PressedButton()
			keep = pressed || s.FocusVisible()
		})
		return keep
	})
}

func (a *app) playerHoverPanel(c *ui.Context, button ui.Element, key, label string, open *bool, body func() bool) {
	trigger := button.Hovered() || button.FocusVisible()
	if !trigger && !*open {
		return
	}
	pop := popoverColors(a.pal.dark)
	ui.Overlay(c, func() {
		// Touch the button so the pointer can reach the panel without leaving both.
		panel := ui.Column(c.Key(key)).AttachTo(button, ui.AnchorTop, ui.AnchorBottom).
			Padding(8, 12).Radius(12).Background(pop.fill).Border(0.5, pop.edge).
			Shadow(0, 8, 24, -4, ui.RGBA(0, 0, 0, 0.2)).Role(ui.RoleGroup).Label(label)
		var keep bool
		panel.Children(func() { keep = body() })
		hovered := panel.Hovered()
		*open = trigger || hovered || keep
		if !*open {
			c.Invalidate()
		}
	})
}

func (a *app) automixDetails(c *ui.Context) {
	info := a.pl.AutomixInfo()
	mode := a.settings.demucsMode()
	status := T("Waiting for the next ready track")
	switch {
	case info.Active && info.Bars > 0:
		status = T("Beat-matched crossfade")
	case info.Active:
		status = T("Standard crossfade")
	case !a.settings.Automix:
		status = T("Automix is off")
	case info.Queued && info.Bars > 0:
		status = T("Preparing beat match")
	case info.Queued:
		status = T("Preparing standard crossfade")
	case mode == player.DemucsOff || mode == player.DemucsCPU || a.settings.dolbyProfile().Spatial:
		status = T("Requires CUDA and a stereo preset")
	case info.PreloadFailed:
		status = T("Could not preload the next track")
	case info.Preloading && info.Progress > 0:
		status = Tf("Preloading the next track · %d%%", int(info.Progress*100))
	case info.Preloading:
		status = T("Preloading the next track")
	case info.Ready:
		status = T("Next track ready")
	case info.Next == "":
		status = T("No next track")
	}
	if info.Preloading && a.settings.Automix {
		c.After(400 * time.Millisecond)
	}
	p := a.pal
	ui.Column(c).Width(196).Gap(4).Children(func() {
		ui.Text(c, status).FontSize(12).FontWeight(600).TextColor(p.text)
		if info.Next != "" && a.settings.Automix {
			ui.Text(c, Tf("Next up · %s", info.Next)).SingleLine().FontSize(11).TextColor(p.muted)
		}
		if info.Estimated || info.Queued {
			ui.Text(c, automixPointLabel(info.Start, info.Estimated)).FontSize(11).TextColor(p.muted)
		}
		if info.Bars > 0 {
			ui.Textf(c, "%.1f → %.1f BPM", info.FromBPM, info.ToBPM).FontFeatures("tnum").FontSize(11).TextColor(p.muted)
			ui.Text(c, Tf("Crossfade %.1f s · Bars %d", info.Overlap.Seconds(), info.Bars)).FontSize(11).TextColor(p.muted)
			ui.Text(c, Tf("Next track cue %s", clock(info.Cue))).FontSize(11).TextColor(p.muted)
		} else if info.Queued {
			ui.Text(c, Tf("Crossfade %.1f s", info.Overlap.Seconds())).FontSize(11).TextColor(p.muted)
			ui.Text(c, Tf("Next track cue %s", clock(info.Cue))).FontSize(11).TextColor(p.muted)
		}
	})
}

func automixPointLabel(point time.Duration, estimated bool) string {
	if estimated {
		return Tf("Estimated transition at %s", clock(point))
	}
	return Tf("Transition at %s", clock(point))
}
