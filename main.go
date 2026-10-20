// Yeti Mute Talker
//
// A small Windows program for Blue Yeti owners. It announces "Microphone muted"
// or "Microphone live" whenever the mute button on the mic is pressed.
//
// The Yeti's mute button works inside the microphone and never tells Windows,
// so this program listens to the mic instead: a live mic always picks up a tiny
// bit of room sound, while a muted Yeti sends complete silence.
//
// The program has no window. It lives in the notification area (system tray)
// and is controlled with global hotkeys or the tray menu.
package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const appName = "Yeti Mute Talker"
const appVersion = "1.0.0"

// ---------------------------------------------------------------------------
// Settings (stored in %APPDATA%\YetiMuteTalker\settings.ini)
// ---------------------------------------------------------------------------

type Settings struct {
	DeviceMatch string
	MutedText   string
	LiveText    string
	SpeechRate  int
	Beeps       bool
	SilenceMs   int
	LiveChunks  int     // how many 200 ms stretches in a row must have sound to count as live
	Threshold   float64 // dBFS switch point, NaN until setup has run
}

func defaultSettings() Settings {
	return Settings{
		DeviceMatch: "Yeti",
		MutedText:   "Microphone muted",
		LiveText:    "Microphone live",
		SpeechRate:  2,
		Beeps:       true,
		SilenceMs:   400,
		LiveChunks:  3,
		Threshold:   math.NaN(),
	}
}

var dataDir string

func settingsPath() string { return filepath.Join(dataDir, "settings.ini") }
func logPath() string      { return filepath.Join(dataDir, "YetiMuteTalker.log") }

func loadSettings() Settings {
	s := defaultSettings()
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return s
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "device":
			s.DeviceMatch = v
		case "muted_text":
			if v != "" {
				s.MutedText = v
			}
		case "live_text":
			if v != "" {
				s.LiveText = v
			}
		case "speech_rate":
			if n, err := strconv.Atoi(v); err == nil && n >= -10 && n <= 10 {
				s.SpeechRate = n
			}
		case "beeps":
			s.Beeps = !(strings.EqualFold(v, "false") || v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "no"))
		case "silence_ms":
			if n, err := strconv.Atoi(v); err == nil && n >= 100 && n <= 5000 {
				s.SilenceMs = n
			}
		case "live_chunks":
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 10 {
				s.LiveChunks = n
			}
		case "threshold":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				s.Threshold = f
			}
		}
	}
	return s
}

func saveSettings(s Settings) {
	th := ""
	if !math.IsNaN(s.Threshold) {
		th = strconv.FormatFloat(s.Threshold, 'f', 2, 64)
	}
	text := fmt.Sprintf(`; Yeti Mute Talker settings
; You can edit this file in Notepad. Restart Yeti Mute Talker afterwards.

; Part of the microphone's name as Windows shows it
device=%s

; What to say when the mic is muted or live
muted_text=%s
live_text=%s

; Speech speed, from -10 (slow) to 10 (fast)
speech_rate=%d

; Play beeps before speaking: true or false
beeps=%t

; How long the mic must be completely silent to count as muted (milliseconds)
silence_ms=%d

; How many 200 millisecond stretches in a row must have sound before the mic
; counts as live again. Higher ignores bumps and touches better, but unmuting
; is announced a little later. 3 means about 0.6 seconds.
live_chunks=%d

; Set by the setup. Delete this line to run the setup again.
threshold=%s
`, s.DeviceMatch, s.MutedText, s.LiveText, s.SpeechRate, s.Beeps, s.SilenceMs, s.LiveChunks, th)
	text = strings.ReplaceAll(text, "\n", "\r\n")
	if err := os.WriteFile(settingsPath(), []byte(text), 0644); err != nil {
		logf("Could not save settings: %v", err)
	}
}

func logf(format string, args ...interface{}) {
	f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\r\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// Microphone: listens to the mic and reports the loudest moment recently
// ---------------------------------------------------------------------------

type levelPoint struct {
	t  time.Time
	db float64
}

type Mic struct {
	name        string
	fallback    bool
	device      *wca.IMMDevice
	volume      *wca.IAudioEndpointVolume
	client      *wca.IAudioClient
	capture     *wca.IAudioCaptureClient
	blockAlign  int
	bits        int
	isFloat     bool
	history     []levelPoint
	windowMs    int
	Level       float64 // loudest moment within the last windowMs, in dBFS
	WindowsMute bool
	LastError   string
}

func hresultOK(err error) bool {
	if err == nil {
		return true
	}
	if oe, ok := err.(*ole.OleError); ok {
		return int32(oe.Code()) >= 0 // success codes such as "buffer empty"
	}
	return false
}

func deviceName(d *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := d.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return ""
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return ""
	}
	return pv.String()
}

// openMic finds the microphone whose name contains match (or the default
// microphone if none does) and starts listening to it.
func openMic(match string, windowMs int) (*Mic, error) {
	var en *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &en); err != nil {
		return nil, fmt.Errorf("device enumerator: %v", err)
	}
	defer en.Release()

	m := &Mic{windowMs: windowMs, Level: -150}
	var col *wca.IMMDeviceCollection
	if err := en.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &col); err == nil {
		var count uint32
		col.GetCount(&count)
		for i := uint32(0); i < count && m.device == nil; i++ {
			var d *wca.IMMDevice
			if col.Item(i, &d) != nil {
				continue
			}
			n := deviceName(d)
			if match != "" && strings.Contains(strings.ToLower(n), strings.ToLower(match)) {
				m.device, m.name = d, n
			} else {
				d.Release()
			}
		}
		col.Release()
	}
	if m.device == nil {
		var d *wca.IMMDevice
		if err := en.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &d); err != nil {
			return nil, fmt.Errorf("no microphone found")
		}
		m.device, m.name, m.fallback = d, deviceName(d), true
	}

	if err := m.device.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &m.volume); err != nil {
		m.Close()
		return nil, fmt.Errorf("endpoint volume: %v", err)
	}
	if err := m.device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &m.client); err != nil {
		m.Close()
		return nil, fmt.Errorf("audio client: %v", err)
	}
	var wfx *wca.WAVEFORMATEX
	if err := m.client.GetMixFormat(&wfx); err != nil {
		m.Close()
		return nil, fmt.Errorf("mix format: %v", err)
	}
	m.blockAlign = int(wfx.NBlockAlign)
	m.bits = int(wfx.WBitsPerSample)
	m.isFloat = wfx.WFormatTag == 3
	if wfx.WFormatTag == 0xFFFE && wfx.CbSize >= 22 {
		sub := *(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(wfx)) + 24))
		m.isFloat = sub == 3
	}
	err := m.client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, 10000000, 0, wfx, nil)
	ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("initialize: %v", err)
	}
	if err := m.client.GetService(wca.IID_IAudioCaptureClient, &m.capture); err != nil {
		m.Close()
		return nil, fmt.Errorf("capture client: %v", err)
	}
	if err := m.client.Start(); err != nil {
		m.Close()
		return nil, fmt.Errorf("start: %v", err)
	}
	return m, nil
}

func (m *Mic) Close() {
	if m.client != nil {
		m.client.Stop()
	}
	if m.capture != nil {
		m.capture.Release()
		m.capture = nil
	}
	if m.client != nil {
		m.client.Release()
		m.client = nil
	}
	if m.volume != nil {
		m.volume.Release()
		m.volume = nil
	}
	if m.device != nil {
		m.device.Release()
		m.device = nil
	}
}

func (m *Mic) sample(b []byte, i int) float64 {
	switch {
	case m.isFloat && m.bits == 32:
		return float64(math.Float32frombits(uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24))
	case m.bits == 16:
		return float64(int16(uint16(b[i])|uint16(b[i+1])<<8)) / 32768.0
	case m.bits == 24:
		v := int32(uint32(b[i])<<8|uint32(b[i+1])<<16|uint32(b[i+2])<<24) >> 8
		return float64(v) / 8388608.0
	case m.bits == 32:
		return float64(int32(uint32(b[i])|uint32(b[i+1])<<8|uint32(b[i+2])<<16|uint32(b[i+3])<<24)) / 2147483648.0
	}
	return 0
}

// Update reads the audio that has arrived. It returns false if the mic is gone.
func (m *Mic) Update() bool {
	if m.capture == nil {
		return false
	}
	var muted bool
	if err := m.volume.GetMute(&muted); err != nil {
		m.LastError = "GetMute: " + err.Error()
		return false
	}
	m.WindowsMute = muted
	bytesPerSample := m.bits / 8
	if bytesPerSample == 0 {
		bytesPerSample = 2
	}
	now := time.Now()
	for {
		var packet uint32
		if err := m.capture.GetNextPacketSize(&packet); !hresultOK(err) {
			m.LastError = "GetNextPacketSize: " + err.Error()
			return false
		}
		if packet == 0 {
			break
		}
		var data *byte
		var frames, flags uint32
		var p1, p2 uint64
		err := m.capture.GetBuffer(&data, &frames, &flags, &p1, &p2)
		if !hresultOK(err) {
			m.LastError = "GetBuffer: " + err.Error()
			return false
		}
		if err != nil { // success code: nothing to read right now
			break
		}
		n := int(frames) * m.blockAlign
		peak := 0.0
		if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT == 0 && n > 0 && data != nil {
			buf := unsafe.Slice(data, n)
			for i := 0; i+bytesPerSample <= n; i += bytesPerSample {
				v := math.Abs(m.sample(buf, i))
				if v > peak {
					peak = v
				}
			}
		}
		m.capture.ReleaseBuffer(frames)
		if frames > 0 {
			db := -150.0
			if peak > 0 {
				db = math.Max(-150, 20*math.Log10(peak))
			}
			m.history = append(m.history, levelPoint{now, db})
		}
	}
	keepMs := m.windowMs
	if keepMs < 2500 {
		keepMs = 2500
	}
	cutoff := now.Add(-time.Duration(keepMs) * time.Millisecond)
	keep := 0
	for keep < len(m.history) && m.history[keep].t.Before(cutoff) {
		keep++
	}
	m.history = m.history[keep:]
	recent := now.Add(-time.Duration(m.windowMs) * time.Millisecond)
	loudest := -150.0
	for _, p := range m.history {
		if p.t.After(recent) && p.db > loudest {
			loudest = p.db
		}
	}
	m.Level = loudest
	return true
}

// SoundInEveryChunk reports whether each of the last n stretches of 200 ms
// contained some sound above threshold. A bump or a touch is short, so it
// can't fill several stretches in a row; real room sound on a live mic does.
func (m *Mic) SoundInEveryChunk(threshold float64, n int) bool {
	now := time.Now()
	for c := 0; c < n; c++ {
		end := now.Add(-time.Duration(c*200) * time.Millisecond)
		start := end.Add(-200 * time.Millisecond)
		found := false
		for _, p := range m.history {
			if p.t.After(start) && !p.t.After(end) && p.db > threshold {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Speech (Windows SAPI voice) and beeps
// ---------------------------------------------------------------------------

var voice *ole.IDispatch

func initVoice(rate int) {
	unk, err := oleutil.CreateObject("SAPI.SpVoice")
	if err != nil {
		logf("Speech not available: %v", err)
		return
	}
	voice, err = unk.QueryInterface(ole.IID_IDispatch)
	unk.Release()
	if err != nil {
		logf("Speech not available: %v", err)
		voice = nil
		return
	}
	oleutil.PutProperty(voice, "Rate", rate)
}

// say speaks text without waiting, cutting off anything still being said.
func say(text string) {
	if voice == nil {
		return
	}
	oleutil.CallMethod(voice, "Speak", text, 3) // async + purge
}

func speechDone() bool {
	if voice == nil {
		return true
	}
	r, err := oleutil.CallMethod(voice, "WaitUntilDone", 0)
	if err != nil {
		return true
	}
	return r.Value() == true
}

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	procBeep = kernel32.NewProc("Beep")
)

func beep(freq, ms int) { procBeep.Call(uintptr(freq), uintptr(ms)) }

// ---------------------------------------------------------------------------
// The app: state, setup, announcements
// ---------------------------------------------------------------------------

const (
	stateUnknown = -1
	stateLive    = 0
	stateMuted   = 1
)

type setupStep int

const (
	setupOff setupStep = iota
	setupIntro
	setupMeasureFirst
	setupPrompt
	setupMeasureSecond
	setupFailed
)

type App struct {
	settings     Settings
	mic          *Mic
	last         int
	started      bool
	retryAt      time.Time
	setup        setupStep
	setupUntil   time.Time
	setupLevels  []float64
	firstLevel   float64
	waitingVoice bool
}

var app App

func (a *App) stateText(s int) string {
	switch s {
	case stateMuted:
		return a.settings.MutedText
	case stateLive:
		return a.settings.LiveText
	}
	return "Microphone not found"
}

// rawState decides muted or live. Going from muted to live needs steady sound
// (not just a bump or a touch); going from live to muted needs a short stretch
// of complete silence.
func (a *App) rawState() int {
	if a.mic.WindowsMute {
		return stateMuted
	}
	silent := a.mic.Level < a.settings.Threshold
	steady := a.mic.SoundInEveryChunk(a.settings.Threshold, a.settings.LiveChunks)
	switch a.last {
	case stateMuted:
		if steady {
			return stateLive
		}
		return stateMuted
	case stateLive:
		if silent {
			return stateMuted
		}
		return stateLive
	}
	// Unknown (just started or reconnected): best guess right now
	if silent {
		return stateMuted
	}
	return stateLive
}

func (a *App) announce(s int) {
	if a.settings.Beeps {
		if s == stateMuted {
			beep(700, 110)
			beep(400, 180)
		} else if s == stateLive {
			beep(500, 110)
			beep(900, 180)
		}
	}
	say(a.stateText(s))
	updateTip()
}

func levelWords(db float64) string {
	if db <= -149 {
		return "complete silence"
	}
	return fmt.Sprintf("minus %d decibels", int(math.Round(-db)))
}

func (a *App) connect() bool {
	m, err := openMic(a.settings.DeviceMatch, a.settings.SilenceMs)
	if err != nil {
		logf("Could not open microphone: %v", err)
		return false
	}
	a.mic = m
	logf("Listening to: %s", m.name)
	return true
}

func (a *App) dropMic(reason string) {
	if a.mic != nil {
		logf("Microphone lost: %s", reason)
		a.mic.Close()
		a.mic = nil
	}
}

func (a *App) startSetup() {
	a.setup = setupIntro
	a.waitingVoice = true
	a.setupUntil = time.Time{}
	say("Setup. This teaches me what your Yeti sounds like when it is muted and when it is live. Please stay quiet until I say done. Measuring now.")
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// tickSetup moves the spoken setup along. Called on every timer tick.
func (a *App) tickSetup(now time.Time) {
	if a.waitingVoice {
		if !speechDone() {
			return
		}
		a.waitingVoice = false
		switch a.setup {
		case setupIntro:
			a.setupUntil = now.Add(400 * time.Millisecond)
		case setupPrompt:
			a.setupUntil = now.Add(2500 * time.Millisecond)
		}
		return
	}
	switch a.setup {
	case setupIntro, setupPrompt:
		if now.After(a.setupUntil) {
			a.setupLevels = nil
			a.setupUntil = now.Add(3 * time.Second)
			if a.setup == setupIntro {
				a.setup = setupMeasureFirst
			} else {
				a.setup = setupMeasureSecond
			}
		}
	case setupMeasureFirst, setupMeasureSecond:
		a.setupLevels = append(a.setupLevels, a.mic.Level)
		if now.Before(a.setupUntil) {
			return
		}
		lvl := median(a.setupLevels)
		if a.setup == setupMeasureFirst {
			a.firstLevel = lvl
			if a.settings.Beeps {
				beep(800, 150)
			}
			a.setup = setupPrompt
			a.waitingVoice = true
			say("Now press the mute button on your microphone once, then stay quiet.")
			return
		}
		first, second := a.firstLevel, lvl
		live, muted := math.Max(first, second), math.Min(first, second)
		logf("Setup: first %.1f dB, second %.1f dB", first, second)
		if live-muted < 6 {
			a.setup = setupFailed
			say(fmt.Sprintf("I couldn't hear a difference. The first level was %s and the second was %s. Press Control Alt C to try again.", levelWords(first), levelWords(second)))
			return
		}
		a.settings.Threshold = (live + muted) / 2
		saveSettings(a.settings)
		a.setup = setupOff
		if second < first {
			a.last = stateMuted
		} else {
			a.last = stateLive
		}
		a.started = true
		say("Done. Your mic is now " + map[int]string{stateMuted: "muted", stateLive: "live"}[a.last] + ". Yeti Mute Talker is in the notification area.")
		updateTip()
	}
}

// tick runs about 20 times a second.
func (a *App) tick() {
	now := time.Now()
	if a.mic == nil {
		if now.Before(a.retryAt) {
			return
		}
		a.retryAt = now.Add(2 * time.Second)
		if !a.connect() {
			if !a.started {
				a.started = true
				a.last = stateUnknown
				say(appName + " is running. Waiting for the microphone.")
				updateTip()
			}
			return
		}
		// Give the stream a moment before judging it
		a.retryAt = now.Add(500 * time.Millisecond)
		a.mic.Update()
		return
	}
	if !a.mic.Update() {
		a.dropMic(a.mic.LastError)
		if a.setup != setupOff && a.setup != setupFailed {
			a.setup = setupFailed
			say("The microphone was disconnected. Setup stopped. Press Control Alt C to try again.")
		} else if a.last != stateUnknown {
			say("Microphone disconnected")
		}
		a.last = stateUnknown
		updateTip()
		return
	}
	if now.Before(a.retryAt) { // just connected, let audio arrive
		return
	}
	if a.setup != setupOff && a.setup != setupFailed {
		a.tickSetup(now)
		return
	}
	if math.IsNaN(a.settings.Threshold) {
		if a.setup == setupOff {
			if a.mic.fallback {
				logf("No mic named %q, using %s", a.settings.DeviceMatch, a.mic.name)
			}
			a.startSetup()
		}
		return
	}
	raw := a.rawState()
	switch {
	case !a.started:
		a.started = true
		a.last = raw
		msg := appName + " is running. "
		if a.mic.fallback {
			msg += "Yeti not found, using " + a.mic.name + ". "
		}
		say(msg + a.stateText(raw))
		updateTip()
	case a.last == stateUnknown:
		a.last = raw
		say("Microphone connected. " + a.stateText(raw))
		updateTip()
	case raw != a.last:
		a.last = raw
		a.announce(raw)
	}
}

func (a *App) sayLevel() {
	if a.mic == nil {
		say("Microphone not found")
		return
	}
	msg := "Sound level " + levelWords(a.mic.Level) + "."
	if !math.IsNaN(a.settings.Threshold) {
		msg += " Switch point " + levelWords(a.settings.Threshold) + "."
	}
	say(msg)
}

func (a *App) redoSetup() {
	if a.mic == nil {
		say("Microphone not found. Plug in your microphone and try again.")
		return
	}
	a.startSetup()
}

// ---------------------------------------------------------------------------
// Start with Windows
// ---------------------------------------------------------------------------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "YetiMuteTalker"

func startsWithWindows() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

func setStartsWithWindows(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return k.SetStringValue(runValue, `"`+exe+`"`)
	}
	err = k.DeleteValue(runValue)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Windows plumbing: hidden window, tray icon, menu, hotkeys, timer
// ---------------------------------------------------------------------------

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	pRegisterClassEx   = user32.NewProc("RegisterClassExW")
	pCreateWindowEx    = user32.NewProc("CreateWindowExW")
	pDefWindowProc     = user32.NewProc("DefWindowProcW")
	pGetMessage        = user32.NewProc("GetMessageW")
	pTranslateMessage  = user32.NewProc("TranslateMessage")
	pDispatchMessage   = user32.NewProc("DispatchMessageW")
	pPostQuitMessage   = user32.NewProc("PostQuitMessage")
	pPostMessage       = user32.NewProc("PostMessageW")
	pRegisterHotKey    = user32.NewProc("RegisterHotKey")
	pUnregisterHotKey  = user32.NewProc("UnregisterHotKey")
	pSetTimer          = user32.NewProc("SetTimer")
	pCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	pAppendMenu        = user32.NewProc("AppendMenuW")
	pTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	pDestroyMenu       = user32.NewProc("DestroyMenu")
	pSetForeground     = user32.NewProc("SetForegroundWindow")
	pGetCursorPos      = user32.NewProc("GetCursorPos")
	pLoadIcon          = user32.NewProc("LoadIconW")
	pFindWindow        = user32.NewProc("FindWindowW")
	pRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")
	pDestroyWindow     = user32.NewProc("DestroyWindow")
	pShellNotifyIcon   = shell32.NewProc("Shell_NotifyIconW")
	pExtractIconEx     = shell32.NewProc("ExtractIconExW")
)

const (
	wmDestroy     = 0x0002
	wmCommand     = 0x0111
	wmTimer       = 0x0113
	wmHotkey      = 0x0312
	wmContextMenu = 0x007B
	wmNull        = 0x0000
	wmApp         = 0x8000
	wmTray        = wmApp + 1
	wmAnotherCopy = wmApp + 2
	ninSelect     = 0x0400
	ninKeySelect  = 0x0401

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4
	nifMessage    = 0x1
	nifIcon       = 0x2
	nifTip        = 0x4
	nifShowTip    = 0x80

	mfString    = 0x0
	mfSeparator = 0x800
	mfChecked   = 0x8
	tpmReturn   = 0x0100
	tpmRight    = 0x0008
	tpmBottom   = 0x0020

	modAlt     = 0x1
	modControl = 0x2
	modShift   = 0x4
	modNoRep   = 0x4000

	cmdStatus  = 1
	cmdLevel   = 2
	cmdSetup   = 3
	cmdBeeps   = 4
	cmdStartup = 5
	cmdFolder  = 6
	cmdExit    = 7

	className = "YetiMuteTalkerWindow"
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msgT struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	private uint32
}

type notifyIconData struct {
	Size            uint32
	Wnd             windows.Handle
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        windows.GUID
	BalloonIcon     windows.Handle
}

var (
	hwnd           windows.Handle
	trayIcon       windows.Handle
	taskbarCreated uint32
	hotkeyProblems []string
)

func u16(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }

func trayData() *notifyIconData {
	nid := &notifyIconData{}
	nid.Size = uint32(unsafe.Sizeof(*nid))
	nid.Wnd = hwnd
	nid.ID = 1
	return nid
}

func setTip(nid *notifyIconData, text string) {
	t, _ := windows.UTF16FromString(text)
	if len(t) > len(nid.Tip) {
		t = t[:len(nid.Tip)-1]
		t = append(t, 0)
	}
	copy(nid.Tip[:], t)
}

func tipText() string {
	switch app.last {
	case stateMuted:
		return appName + ": " + app.settings.MutedText
	case stateLive:
		return appName + ": " + app.settings.LiveText
	}
	return appName + ": Microphone not found"
}

func addTray() {
	nid := trayData()
	nid.Flags = nifMessage | nifIcon | nifTip | nifShowTip
	nid.CallbackMessage = wmTray
	nid.Icon = trayIcon
	setTip(nid, tipText())
	pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(nid)))
	nid.Version = 4
	pShellNotifyIcon.Call(nimSetVersion, uintptr(unsafe.Pointer(nid)))
}

func updateTip() {
	if hwnd == 0 {
		return
	}
	nid := trayData()
	nid.Flags = nifTip | nifShowTip
	setTip(nid, tipText())
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(nid)))
}

func removeTray() {
	nid := trayData()
	pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(nid)))
}

var menuClosedAt time.Time

func showMenu(x, y int32) {
	menu, _, _ := pCreatePopupMenu.Call()
	add := func(id int, text string, checked bool) {
		flags := uintptr(mfString)
		if checked {
			flags |= mfChecked
		}
		pAppendMenu.Call(menu, flags, uintptr(id), uintptr(unsafe.Pointer(u16(text))))
	}
	sep := func() { pAppendMenu.Call(menu, mfSeparator, 0, 0) }

	status := "Say microphone status"
	if app.last == stateMuted || app.last == stateLive {
		status = "Microphone is " + map[int]string{stateMuted: "muted", stateLive: "live"}[app.last]
	}
	add(cmdStatus, status+"\tCtrl+Alt+M", false)
	add(cmdLevel, "Say sound level\tCtrl+Alt+L", false)
	add(cmdSetup, "Run setup again\tCtrl+Alt+C", false)
	sep()
	add(cmdBeeps, "Play beeps", app.settings.Beeps)
	add(cmdStartup, "Start with Windows", startsWithWindows())
	add(cmdFolder, "Open settings folder", false)
	sep()
	add(cmdExit, "Exit\tCtrl+Alt+Shift+M", false)

	if x == 0 && y == 0 {
		var pt struct{ X, Y int32 }
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		x, y = pt.X, pt.Y
	}
	pSetForeground.Call(uintptr(hwnd))
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmReturn|tpmRight|tpmBottom, uintptr(x), uintptr(y), 0, uintptr(hwnd), 0)
	pPostMessage.Call(uintptr(hwnd), wmNull, 0, 0)
	pDestroyMenu.Call(menu)
	menuClosedAt = time.Now()
	if cmd != 0 {
		runCommand(int(cmd))
	}
}

func runCommand(cmd int) {
	switch cmd {
	case cmdStatus:
		app.announce(app.last)
	case cmdLevel:
		app.sayLevel()
	case cmdSetup:
		app.redoSetup()
	case cmdBeeps:
		app.settings.Beeps = !app.settings.Beeps
		saveSettings(app.settings)
		if app.settings.Beeps {
			say("Beeps on")
		} else {
			say("Beeps off")
		}
	case cmdStartup:
		on := !startsWithWindows()
		if err := setStartsWithWindows(on); err != nil {
			logf("Start with Windows: %v", err)
			say("Sorry, that didn't work.")
		} else if on {
			say("Yeti Mute Talker will start with Windows")
		} else {
			say("Yeti Mute Talker will no longer start with Windows")
		}
	case cmdFolder:
		windows.ShellExecute(0, u16("open"), u16(dataDir), nil, nil, 1)
	case cmdExit:
		quit()
	}
}

func quit() {
	if voice != nil {
		oleutil.CallMethod(voice, "Speak", "Yeti Mute Talker closed", 2) // wait until spoken
	}
	pDestroyWindow.Call(uintptr(hwnd))
}

func wndProc(h windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTimer:
		app.tick()
		return 0
	case wmHotkey:
		switch wParam {
		case 1:
			app.announce(app.last)
		case 2:
			app.sayLevel()
		case 3:
			app.redoSetup()
		case 4:
			quit()
		}
		return 0
	case wmTray:
		switch uint32(lParam & 0xFFFF) {
		case wmContextMenu, ninSelect, ninKeySelect:
			// Windows can report one key press twice, so ignore repeats
			if time.Since(menuClosedAt) > 400*time.Millisecond {
				showMenu(int32(int16(wParam&0xFFFF)), int32(int16((wParam>>16)&0xFFFF)))
			}
		}
		return 0
	case wmAnotherCopy:
		say("Yeti Mute Talker is already running. " + app.stateText(app.last))
		return 0
	case wmDestroy:
		removeTray()
		for i := 1; i <= 4; i++ {
			pUnregisterHotKey.Call(uintptr(h), uintptr(i))
		}
		pPostQuitMessage.Call(0)
		return 0
	}
	if taskbarCreated != 0 && msg == taskbarCreated {
		addTray() // Explorer restarted
		return 0
	}
	r, _, _ := pDefWindowProc.Call(uintptr(h), uintptr(msg), wParam, lParam)
	return r
}

func main() {
	runtime.LockOSThread()

	// Only one copy at a time: if one is already running, ask it to speak.
	if w, _, _ := pFindWindow.Call(uintptr(unsafe.Pointer(u16(className))), 0); w != 0 {
		pPostMessage.Call(w, wmAnotherCopy, 0, 0)
		return
	}

	appData, _ := os.UserConfigDir()
	dataDir = filepath.Join(appData, "YetiMuteTalker")
	os.MkdirAll(dataDir, 0755)

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || int32(oe.Code()) < 0 {
			logf("COM init failed: %v", err)
		}
	}
	defer ole.CoUninitialize()

	app.settings = loadSettings()
	app.last = stateUnknown
	if _, err := os.Stat(settingsPath()); err != nil {
		saveSettings(app.settings) // write defaults so they can be edited
	}
	logf("%s %s starting", appName, appVersion)
	initVoice(app.settings.SpeechRate)

	inst, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	exe, _ := os.Executable()
	var small windows.Handle
	pExtractIconEx.Call(uintptr(unsafe.Pointer(u16(exe))), 0, 0, uintptr(unsafe.Pointer(&small)), 1)
	trayIcon = small
	if trayIcon == 0 {
		ic, _, _ := pLoadIcon.Call(0, 32512) // IDI_APPLICATION
		trayIcon = windows.Handle(ic)
	}

	wc := wndClassEx{
		WndProc:   syscall.NewCallback(wndProc),
		Instance:  windows.Handle(inst),
		ClassName: u16(className),
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	h, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(u16(className))), uintptr(unsafe.Pointer(u16(appName))),
		0, 0, 0, 0, 0, 0, 0, inst, 0)
	if h == 0 {
		logf("Could not create window: %v", err)
		return
	}
	hwnd = windows.Handle(h)
	tc, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	taskbarCreated = uint32(tc)

	hotkeys := []struct {
		id   int
		mods uintptr
		key  uintptr
		name string
	}{
		{1, modControl | modAlt, 'M', "Control Alt M"},
		{2, modControl | modAlt, 'L', "Control Alt L"},
		{3, modControl | modAlt, 'C', "Control Alt C"},
		{4, modControl | modAlt | modShift, 'M', "Control Alt Shift M"},
	}
	for _, hk := range hotkeys {
		if r, _, _ := pRegisterHotKey.Call(h, uintptr(hk.id), hk.mods|modNoRep, hk.key); r == 0 {
			hotkeyProblems = append(hotkeyProblems, hk.name)
		}
	}
	if len(hotkeyProblems) > 0 {
		logf("Hotkeys already used by another program: %s", strings.Join(hotkeyProblems, ", "))
	}

	addTray()
	pSetTimer.Call(h, 1, 50, 0)

	var m msgT
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	if app.mic != nil {
		app.mic.Close()
	}
	logf("%s closed", appName)
}
