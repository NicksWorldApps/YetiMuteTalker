package main

// Speech output through the Prism library (prism.dll, shipped next to the
// program). Prism talks to NVDA and JAWS directly, and to the Windows voices
// (SAPI and OneCore) when no screen reader is wanted or running.
//
// Prism: https://github.com/ethindp/prism (Mozilla Public License 2.0)

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

const (
	prismBackendSAPI    uint64 = 0x1D6DF72422CEEE66
	prismBackendNVDA    uint64 = 0x89CC19C5C4AC1A56
	prismBackendJAWS    uint64 = 0xAC3D60E9BD84B53E
	prismBackendOneCore uint64 = 0x6797D32F0D994CB4

	prismSupportedNow    uint64 = 1 << 0
	prismSupportsOutput  uint64 = 1 << 5
	prismSupportsSpeakng uint64 = 1 << 6
	prismSupportsRate    uint64 = 1 << 12
)

var (
	prismDLL                                                  *windows.DLL
	pInit, pCreate, pFree, pInitialize, pFeatures, pSpeak     *windows.Proc
	pOutput, pIsSpeaking, pSetRate, pCountVoices, pVoiceName  *windows.Proc
	pSetVoice, pGetVoice, pRefreshVoices, pStop, pErrorString *windows.Proc
	prismCtx                                                  uintptr
)

// cstr makes a NUL-terminated UTF-8 string for Prism.
func cstr(s string) []byte { return append([]byte(s), 0) }

func gostr(p uintptr) string {
	if p == 0 {
		return ""
	}
	return windows.BytePtrToString((*byte)(unsafe.Pointer(p)))
}

func perr(r uintptr) int32 { return int32(uint32(r)) }

func prismErrorText(code int32) string {
	if pErrorString == nil {
		return fmt.Sprint(code)
	}
	r, _, _ := pErrorString.Call(uintptr(code))
	return gostr(r)
}

func loadPrism() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(exe), "prism.dll")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("prism.dll not found next to the program")
	}
	prismDLL, err = windows.LoadDLL(path)
	if err != nil {
		return err
	}
	find := func(name string) *windows.Proc {
		p, e := prismDLL.FindProc(name)
		if e != nil && err == nil {
			err = e
		}
		return p
	}
	pInit = find("prism_init")
	pCreate = find("prism_registry_create")
	pFree = find("prism_backend_free")
	pInitialize = find("prism_backend_initialize")
	pFeatures = find("prism_backend_get_features")
	pSpeak = find("prism_backend_speak")
	pOutput = find("prism_backend_output")
	pIsSpeaking = find("prism_backend_is_speaking")
	pSetRate = find("prism_backend_set_rate")
	pCountVoices = find("prism_backend_count_voices")
	pVoiceName = find("prism_backend_get_voice_name")
	pSetVoice = find("prism_backend_set_voice")
	pGetVoice = find("prism_backend_get_voice")
	pRefreshVoices = find("prism_backend_refresh_voices")
	pStop = find("prism_backend_stop")
	pErrorString = find("prism_error_string")
	if err != nil {
		return err
	}
	prismCtx, _, _ = pInit.Call(0)
	if prismCtx == 0 {
		return fmt.Errorf("prism_init failed")
	}
	return nil
}

// backend is one Prism speech output (NVDA, JAWS, SAPI or OneCore).
type backend struct {
	id          uint64
	name        string
	h           uintptr
	initialized bool
}

func (b *backend) ensureCreated() bool {
	if b.h == 0 {
		b.h, _, _ = pCreate.Call(prismCtx, uintptr(b.id))
	}
	return b.h != 0
}

func (b *backend) features() uint64 {
	if !b.ensureCreated() {
		return 0
	}
	r, _, _ := pFeatures.Call(b.h)
	return uint64(r)
}

// availableNow reports whether this output can be used right now
// (for a screen reader: whether it is running).
func (b *backend) availableNow() bool { return b.features()&prismSupportedNow != 0 }

func (b *backend) ensureInitialized() bool {
	if !b.ensureCreated() {
		return false
	}
	if b.initialized {
		return true
	}
	r, _, _ := pInitialize.Call(b.h)
	if code := perr(r); code != 0 && code != 15 { // 15 = already initialized
		logf("%s could not start: %s", b.name, prismErrorText(code))
		b.reset()
		return false
	}
	b.initialized = true
	return true
}

func (b *backend) reset() {
	if b.h != 0 {
		pFree.Call(b.h)
	}
	b.h = 0
	b.initialized = false
}

// speak says text, interrupting anything this program said before.
// Screen readers also show it on a braille display.
func (b *backend) speak(text string, screenReader bool) bool {
	if !b.ensureInitialized() {
		return false
	}
	t := cstr(text)
	proc := pSpeak
	if screenReader && b.features()&prismSupportsOutput != 0 {
		proc = pOutput
	}
	r, _, _ := proc.Call(b.h, uintptr(unsafe.Pointer(&t[0])), 1)
	runtime.KeepAlive(t)
	if code := perr(r); code != 0 {
		logf("%s could not speak: %s", b.name, prismErrorText(code))
		b.reset() // e.g. the screen reader was restarted; try fresh next time
		return false
	}
	return true
}

func (b *backend) isSpeaking() (speaking bool, known bool) {
	if b.h == 0 || !b.initialized || b.features()&prismSupportsSpeakng == 0 {
		return false, false
	}
	var out uint8
	r, _, _ := pIsSpeaking.Call(b.h, uintptr(unsafe.Pointer(&out)))
	if perr(r) != 0 {
		return false, false
	}
	return out != 0, true
}

func (b *backend) setRate(rate int) {
	if !b.ensureInitialized() || b.features()&prismSupportsRate == 0 {
		return
	}
	f := float32(0.5 + float64(rate)/20.0)
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	pSetRate.Call(b.h, uintptr(*(*uint32)(unsafe.Pointer(&f))))
}

func (b *backend) voices() []string {
	if !b.ensureInitialized() {
		return nil
	}
	var n uintptr
	if r, _, _ := pCountVoices.Call(b.h, uintptr(unsafe.Pointer(&n))); perr(r) != 0 {
		return nil
	}
	list := make([]string, 0, n)
	for i := uintptr(0); i < n; i++ {
		var p uintptr
		if r, _, _ := pVoiceName.Call(b.h, i, uintptr(unsafe.Pointer(&p))); perr(r) != 0 {
			list = append(list, fmt.Sprintf("Voice %d", i+1))
			continue
		}
		list = append(list, gostr(p))
	}
	return list
}

func (b *backend) setVoiceByName(name string) bool {
	for i, v := range b.voices() {
		if strings.EqualFold(v, name) {
			r, _, _ := pSetVoice.Call(b.h, uintptr(i))
			return perr(r) == 0
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The speaker: chooses where each message goes
// ---------------------------------------------------------------------------

const (
	modeAuto    = "auto"    // screen reader if one is running, else Windows voice
	modeWindows = "windows" // always the Windows voice
)

type VoiceChoice struct {
	Engine string // "sapi" or "onecore"
	Name   string
	Label  string
}

type Speaker struct {
	ok       bool
	nvda     backend
	jaws     backend
	sapi     backend
	onecore  backend
	last     *backend
	doneAt   time.Time // estimated end of speech when the output can't tell us
	fallback *ole.IDispatch
}

var speaker Speaker

func (s *Speaker) init() {
	s.nvda = backend{id: prismBackendNVDA, name: "NVDA"}
	s.jaws = backend{id: prismBackendJAWS, name: "JAWS"}
	s.sapi = backend{id: prismBackendSAPI, name: "Windows voice (SAPI)"}
	s.onecore = backend{id: prismBackendOneCore, name: "Windows voice (OneCore)"}
	if err := loadPrism(); err != nil {
		logf("Prism not available, using the basic Windows voice: %v", err)
		s.initFallback()
		return
	}
	s.ok = true
	s.applyVoiceSettings()
}

// initFallback uses the Windows SAPI voice directly if prism.dll is missing.
func (s *Speaker) initFallback() {
	unk, err := oleutil.CreateObject("SAPI.SpVoice")
	if err != nil {
		logf("Speech not available: %v", err)
		return
	}
	s.fallback, _ = unk.QueryInterface(ole.IID_IDispatch)
	unk.Release()
	if s.fallback != nil {
		oleutil.PutProperty(s.fallback, "Rate", app.settings.SpeechRate)
	}
}

func (s *Speaker) windowsVoice() *backend {
	if app.settings.VoiceEngine == "onecore" {
		return &s.onecore
	}
	return &s.sapi
}

func (s *Speaker) applyVoiceSettings() {
	if !s.ok {
		if s.fallback != nil {
			oleutil.PutProperty(s.fallback, "Rate", app.settings.SpeechRate)
		}
		return
	}
	w := s.windowsVoice()
	if app.settings.VoiceName != "" && !w.setVoiceByName(app.settings.VoiceName) {
		logf("Voice %q not found, using the default voice", app.settings.VoiceName)
	}
	w.setRate(app.settings.SpeechRate)
}

// outputName says which output would be used right now.
func (s *Speaker) outputName() string {
	if !s.ok {
		return "Windows voice"
	}
	if app.settings.SpeechMode != modeWindows {
		if s.nvda.availableNow() {
			return "NVDA"
		}
		if s.jaws.availableNow() {
			return "JAWS"
		}
	}
	return "Windows voice"
}

func (s *Speaker) say(text string) {
	s.doneAt = time.Now().Add(time.Duration(600+len(text)*65) * time.Millisecond)
	if !s.ok {
		if s.fallback != nil {
			oleutil.CallMethod(s.fallback, "Speak", text, 3) // async + purge
		}
		return
	}
	if app.settings.SpeechMode != modeWindows {
		for _, sr := range []*backend{&s.nvda, &s.jaws} {
			if sr.availableNow() && sr.speak(text, true) {
				s.stopOther(sr)
				s.last = sr
				return
			}
		}
	}
	w := s.windowsVoice()
	if s.last != w {
		s.stopOther(w)
	}
	if !w.speak(text, false) && w == &s.onecore {
		s.sapi.speak(text, false) // last resort
		w = &s.sapi
	}
	s.last = w
}

// stopOther silences a Windows voice when switching outputs, so two voices
// never talk at once.
func (s *Speaker) stopOther(keep *backend) {
	for _, b := range []*backend{&s.sapi, &s.onecore} {
		if b != keep && b.initialized {
			pStop.Call(b.h)
		}
	}
}

// done reports whether the last message has finished being spoken.
func (s *Speaker) done() bool {
	if !s.ok {
		if s.fallback == nil {
			return true
		}
		r, err := oleutil.CallMethod(s.fallback, "WaitUntilDone", 0)
		return err != nil || r.Value() == true
	}
	if s.last != nil {
		if speaking, known := s.last.isSpeaking(); known {
			return !speaking
		}
	}
	return time.Now().After(s.doneAt)
}

// sayAndWait speaks and waits (up to a few seconds) for it to finish.
func (s *Speaker) sayAndWait(text string) {
	s.say(text)
	limit := time.Now().Add(5 * time.Second)
	time.Sleep(150 * time.Millisecond)
	for !s.done() && time.Now().Before(limit) {
		time.Sleep(50 * time.Millisecond)
	}
}

// voiceChoices lists the Windows voices from both engines.
func (s *Speaker) voiceChoices() []VoiceChoice {
	if !s.ok {
		return nil
	}
	var out []VoiceChoice
	for _, v := range s.sapi.voices() {
		out = append(out, VoiceChoice{"sapi", v, v})
	}
	for _, v := range s.onecore.voices() {
		out = append(out, VoiceChoice{"onecore", v, v + " (newer voice)"})
	}
	return out
}

func say(text string)  { speaker.say(text) }
func speechDone() bool { return speaker.done() }
