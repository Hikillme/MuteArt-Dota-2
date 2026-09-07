package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove        = 0x0001
	mouseEventLeftDown    = 0x0002
	mouseEventLeftUp      = 0x0004
	mouseEventMoveNoCoal  = 0x2000
	mouseEventVirtualDesk = 0x4000
	mouseEventAbsolute    = 0x8000

	keyEventKeyUp = 0x0002

	vkControl   = 0x11
	vkEnter     = 0x0D
	vkBackspace = 0x08
	vkSpace     = 0x20
	vkEscape    = 0x1B

	vkF2 = 0x71
	vkF3 = 0x72
	vkF4 = 0x73
	vkF6 = 0x75
	vkF7 = 0x76
	vkF8 = 0x77
	vkF9 = 0x78

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	processQueryLimitedInformation = 0x1000
)

type PointI struct {
	X int32
	Y int32
}

type mouseInput struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	_           uint32
	DwExtraInfo uintptr
}

type keyboardInput struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	_           uint32
	DwExtraInfo uintptr
}

type inputMouseStruct struct {
	Type uint32
	_    uint32
	Mi   mouseInput
}

type inputKeyboardStruct struct {
	Type uint32
	_    uint32
	Ki   keyboardInput
	Pad  [8]byte
}

type Stroke [][]float64

type Glyph struct {
	Strokes []Stroke `json:"strokes"`
	MinX    float64  `json:"min_x"`
	MaxX    float64  `json:"max_x"`
	Width   float64  `json:"width"`
	MinY    float64  `json:"min_y"`
	MaxY    float64  `json:"max_y"`
}

type MapDetector struct {
	Found bool
	Side string
	X int
	Y int
	Size int
}

type MapCoords struct {
	TopLeft     [2]int `json:"top_left"`
	BottomRight [2]int `json:"bottom_right"`
	Screen      [2]int `json:"screen"`
}

type Config struct {
	MapCoords  *MapCoords `json:"map_coords,omitempty"`
	FontSizePx int        `json:"font_size_px,omitempty"`
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	winmm    = syscall.NewLazyDLL("winmm.dll")

	procSendInput                 = user32.NewProc("SendInput")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procGetForegroundWindow       = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId  = user32.NewProc("GetWindowThreadProcessId")
	procGetAsyncKeyState          = user32.NewProc("GetAsyncKeyState")
	procGetKeyboardLayout         = user32.NewProc("GetKeyboardLayout")
	procGetKeyboardState          = user32.NewProc("GetKeyboardState")
	procMapVirtualKeyExW          = user32.NewProc("MapVirtualKeyExW")
	procToUnicodeEx               = user32.NewProc("ToUnicodeEx")
	procGetCursorPos              = user32.NewProc("GetCursorPos")
	procSetCursorPos              = user32.NewProc("SetCursorPos")
	procSetProcessDPIAware        = user32.NewProc("SetProcessDPIAware")
	procSetProcessDPIAwareContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDC = user32.NewProc("GetDC")
	procReleaseDC = user32.NewProc("ReleaseDC")
	procCreateCompatibleDC = user32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = user32.NewProc("CreateCompatibleBitmap")
	procSelectObject = user32.NewProc("SelectObject")
	procBitBlt = user32.NewProc("BitBlt")

	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP               = kernel32.NewProc("SetConsoleCP")

	procTimeBeginPeriod = winmm.NewProc("timeBeginPeriod")
	procTimeEndPeriod   = winmm.NewProc("timeEndPeriod")
)

type Writer struct {
	running atomic.Bool
	drawing atomic.Bool

	inputBuffer       string
	chatCapture       bool
	wasDotaForeground bool

	// Cache foreground HWND. Querying process name every 1 ms
	// is expensive and can make fast keystrokes get missed.
	lastForegroundHWND   uintptr
	lastForegroundIsDota bool

	screenWidth   int
	screenHeight  int
	virtualX      int
	virtualY      int
	virtualWidth  int
	virtualHeight int

	mapCoords      MapCoords
	pendingTopLeft *[2]int

	configDir  string
	configPath string
	config     Config

	mapSizeRatio    float64
	mapMarginLeft   int
	mapMarginBottom int

	textWidthRatio        float64
	singleLineHeightRatio float64
	multiLineHeightRatio  float64
	letterSpacing         float64
	spaceWidth            float64
	linePitchRatio        float64

	minFontPx         int
	autoMinReadablePx int
	maxFontPx         int
	defaultWrapFontPx int
	fontStepPx        int
	maxAutoLines      int
	maxManualLines    int
	absoluteMinFontPx int

	smallFontThresholdPx  float64
	smallFontSimplifyPx   float64
	smallFontSpacingExtra float64

	drawHz             float64
	frameDelay         time.Duration
	lineStepPx         float64
	shortLineStepPx    float64
	shortLineThreshold float64

	strokeStartDelay time.Duration
	mouseDownDelay   time.Duration
	mouseUpDelay     time.Duration
	strokeEndDelay   time.Duration
	startDrawDelay   time.Duration

	// v1.8 drawing separation delays
	betweenStrokeDelay time.Duration
	betweenLetterDelay time.Duration
	afterLineDelay time.Duration
	beforeNextLetterDelay time.Duration

	ctrlDownGuardDelay   time.Duration
	letterGapDelay       time.Duration
	lineBreakSettleDelay time.Duration
	releaseRepeatDelay   time.Duration
	releaseSettleDelay   time.Duration
	letterSafeLift       float64

	font map[string]Glyph
}

var errDrawInterrupted = errors.New("dota2.exe is not foreground")

var (
	keyboardEvents = make(chan rune, 512)
	keyboardHook uintptr
	keyboardOnce sync.Once
)


const fontJSON = `{"А":{"strokes":[[[0.05,1.0],[0.5,0.0],[0.95,1.0]],[[0.31,0.58],[0.69,0.58]]],"min_x":0.05,"max_x":0.95,"width":0.8999999999999999,"min_y":0.0,"max_y":1.0},"Б":{"strokes":[[[0.08,1.0],[0.08,0.0],[0.92,0.0]],[[0.08,0.48],[0.56,0.48],[0.68,0.5],[0.78,0.54],[0.86,0.6],[0.91,0.68],[0.93,0.76],[0.9,0.84],[0.84,0.91],[0.74,0.96],[0.61,1.0],[0.08,1.0]]],"min_x":0.08,"max_x":0.93,"width":0.8500000000000001,"min_y":0.0,"max_y":1.0},"В":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.08,0.0],[0.55,0.0],[0.67,0.02],[0.77,0.06],[0.85,0.13],[0.9,0.21],[0.91,0.28],[0.88,0.36],[0.81,0.42],[0.71,0.47],[0.58,0.5],[0.08,0.5]],[[0.08,0.5],[0.58,0.5],[0.7,0.52],[0.8,0.56],[0.88,0.63],[0.93,0.71],[0.94,0.78],[0.91,0.86],[0.84,0.92],[0.74,0.97],[0.61,1.0],[0.08,1.0]]],"min_x":0.08,"max_x":0.94,"width":0.86,"min_y":0.0,"max_y":1.0},"Г":{"strokes":[[[0.08,1.0],[0.08,0.0],[0.92,0.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"Д":{"strokes":[[[0.14,0.84],[0.3,0.0],[0.78,0.0],[0.92,0.84]],[[0.02,0.84],[0.98,0.84]],[[0.02,0.82],[0.02,1.06]],[[0.98,0.82],[0.98,1.06]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.0,"max_y":1.06},"Е":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.08,0.0],[0.92,0.0]],[[0.08,0.5],[0.78,0.5]],[[0.08,1.0],[0.92,1.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"Ё":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.08,0.0],[0.92,0.0]],[[0.08,0.5],[0.78,0.5]],[[0.08,1.0],[0.92,1.0]],[[0.26,-0.13],[0.38,-0.13]],[[0.62,-0.13],[0.74,-0.13]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":-0.13,"max_y":1.0},"Ж":{"strokes":[[[0.02,0.0],[0.5,0.5],[0.02,1.0]],[[0.98,0.0],[0.5,0.5],[0.98,1.0]],[[0.5,0.0],[0.5,1.0]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.0,"max_y":1.0},"З":{"strokes":[[[0.1,0.12],[0.2,0.06],[0.32,0.02],[0.66,0.02],[0.77,0.05],[0.86,0.11],[0.91,0.19],[0.92,0.27],[0.88,0.35],[0.79,0.42],[0.66,0.48],[0.56,0.5],[0.67,0.52],[0.8,0.58],[0.89,0.66],[0.94,0.75],[0.92,0.84],[0.86,0.91],[0.76,0.96],[0.64,0.99],[0.3,0.99],[0.18,0.96],[0.08,0.89]]],"min_x":0.08,"max_x":0.94,"width":0.86,"min_y":0.02,"max_y":0.99},"И":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.92,0.0],[0.92,1.0]],[[0.08,0.88],[0.92,0.12]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"Й":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.92,0.0],[0.92,1.0]],[[0.08,0.88],[0.92,0.12]],[[0.28,-0.12],[0.39,-0.06],[0.5,-0.03],[0.61,-0.06],[0.72,-0.12]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":-0.12,"max_y":1.0},"К":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.92,0.02],[0.08,0.52]],[[0.08,0.5],[0.92,0.98]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"Л":{"strokes":[[[0.04,1.0],[0.28,0.0],[0.92,0.0],[0.92,1.0]]],"min_x":0.04,"max_x":0.92,"width":0.88,"min_y":0.0,"max_y":1.0},"М":{"strokes":[[[0.05,1.0],[0.05,0.0],[0.5,0.55],[0.95,0.0],[0.95,1.0]]],"min_x":0.05,"max_x":0.95,"width":0.8999999999999999,"min_y":0.0,"max_y":1.0},"Н":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.92,0.0],[0.92,1.0]],[[0.08,0.5],[0.92,0.5]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"О":{"strokes":[[[0.28,0.02],[0.72,0.02],[0.81,0.05],[0.89,0.11],[0.95,0.2],[0.98,0.31],[0.98,0.69],[0.95,0.8],[0.89,0.89],[0.81,0.95],[0.72,0.98],[0.28,0.98],[0.19,0.95],[0.11,0.89],[0.05,0.8],[0.02,0.69],[0.02,0.31],[0.05,0.2],[0.11,0.11],[0.19,0.05],[0.28,0.02]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.02,"max_y":0.98},"П":{"strokes":[[[0.1,1.0],[0.1,0.0],[0.9,0.0],[0.9,1.0]]],"min_x":0.1,"max_x":0.9,"width":0.8,"min_y":0.0,"max_y":1.0},"Р":{"strokes":[[[0.09,1.0],[0.09,0.0]],[[0.09,0.0],[0.56,0.0],[0.68,0.02],[0.78,0.06],[0.86,0.13],[0.91,0.21],[0.92,0.28],[0.88,0.36],[0.8,0.42],[0.7,0.47],[0.57,0.5],[0.09,0.5]]],"min_x":0.09,"max_x":0.92,"width":0.8300000000000001,"min_y":0.0,"max_y":1.0},"С":{"strokes":[[[0.92,0.12],[0.82,0.06],[0.71,0.03],[0.31,0.03],[0.2,0.06],[0.12,0.12],[0.06,0.22],[0.03,0.34],[0.03,0.66],[0.06,0.78],[0.12,0.88],[0.2,0.94],[0.31,0.97],[0.71,0.97],[0.82,0.94],[0.92,0.88]]],"min_x":0.03,"max_x":0.92,"width":0.89,"min_y":0.03,"max_y":0.97},"Т":{"strokes":[[[0.08,0.0],[0.92,0.0]],[[0.5,0.0],[0.5,1.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"У":{"strokes":[[[0.04,0.0],[0.48,0.56],[0.96,0.0]],[[0.5,0.52],[0.28,1.0]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.0,"max_y":1.0},"Ф":{"strokes":[[[0.28,0.22],[0.72,0.22],[0.82,0.25],[0.9,0.31],[0.96,0.4],[0.98,0.5],[0.96,0.6],[0.9,0.69],[0.82,0.75],[0.72,0.78],[0.28,0.78],[0.18,0.75],[0.1,0.69],[0.04,0.6],[0.02,0.5],[0.04,0.4],[0.1,0.31],[0.18,0.25],[0.28,0.22]],[[0.5,0.0],[0.5,1.0]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.0,"max_y":1.0},"Х":{"strokes":[[[0.04,0.02],[0.96,0.98]],[[0.96,0.02],[0.04,0.98]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.02,"max_y":0.98},"Ц":{"strokes":[[[0.08,0.0],[0.08,0.91]],[[0.84,0.0],[0.84,0.91]],[[0.04,0.91],[0.98,0.91]],[[0.98,0.89],[0.98,1.08]]],"min_x":0.04,"max_x":0.98,"width":0.94,"min_y":0.0,"max_y":1.08},"Ч":{"strokes":[[[0.08,0.0],[0.08,0.39],[0.12,0.45],[0.2,0.51],[0.33,0.55],[0.49,0.56],[0.66,0.53],[0.92,0.42]],[[0.92,0.0],[0.92,1.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"Ш":{"strokes":[[[0.06,0.0],[0.06,1.0]],[[0.5,0.0],[0.5,1.0]],[[0.94,0.0],[0.94,1.0]],[[0.06,1.0],[0.94,1.0]]],"min_x":0.06,"max_x":0.94,"width":0.8799999999999999,"min_y":0.0,"max_y":1.0},"Щ":{"strokes":[[[0.05,0.0],[0.05,0.9]],[[0.45,0.0],[0.45,0.9]],[[0.85,0.0],[0.85,0.9]],[[0.02,0.9],[0.98,0.9]],[[0.98,0.88],[0.98,1.08]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.0,"max_y":1.08},"Ъ":{"strokes":[[[0.02,0.0],[0.36,0.0]],[[0.26,0.0],[0.26,1.0]],[[0.22,0.5],[0.59,0.5],[0.71,0.52],[0.81,0.57],[0.88,0.64],[0.92,0.72],[0.92,0.8],[0.87,0.88],[0.78,0.94],[0.66,0.98],[0.22,1.0]]],"min_x":0.02,"max_x":0.92,"width":0.9,"min_y":0.0,"max_y":1.0},"Ы":{"strokes":[[[0.05,0.0],[0.05,1.0]],[[0.02,0.5],[0.31,0.5],[0.42,0.52],[0.51,0.57],[0.57,0.65],[0.6,0.73],[0.59,0.81],[0.54,0.88],[0.45,0.94],[0.34,0.98],[0.02,1.0]],[[0.91,0.0],[0.91,1.0]]],"min_x":0.02,"max_x":0.91,"width":0.89,"min_y":0.0,"max_y":1.0},"Ь":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.05,0.5],[0.52,0.5],[0.64,0.52],[0.74,0.57],[0.81,0.64],[0.85,0.72],[0.85,0.8],[0.8,0.88],[0.71,0.94],[0.59,0.98],[0.05,1.0]]],"min_x":0.05,"max_x":0.85,"width":0.7999999999999999,"min_y":0.0,"max_y":1.0},"Э":{"strokes":[[[0.08,0.12],[0.19,0.06],[0.3,0.03],[0.7,0.03],[0.81,0.06],[0.89,0.12],[0.95,0.22],[0.98,0.34],[0.98,0.66],[0.95,0.78],[0.89,0.88],[0.81,0.94],[0.7,0.97],[0.3,0.97],[0.19,0.94],[0.08,0.88]],[[0.42,0.5],[0.98,0.5]]],"min_x":0.08,"max_x":0.98,"width":0.9,"min_y":0.03,"max_y":0.97},"Ю":{"strokes":[[[0.04,0.0],[0.04,1.0]],[[0.0,0.5],[0.32,0.5]],[[0.54,0.02],[0.74,0.02],[0.82,0.05],[0.89,0.11],[0.94,0.2],[0.97,0.31],[0.97,0.69],[0.94,0.8],[0.89,0.89],[0.82,0.95],[0.74,0.98],[0.54,0.98],[0.46,0.95],[0.39,0.89],[0.34,0.8],[0.31,0.69],[0.31,0.31],[0.34,0.2],[0.39,0.11],[0.46,0.05],[0.54,0.02]]],"min_x":0.0,"max_x":0.97,"width":0.97,"min_y":0.0,"max_y":1.0},"Я":{"strokes":[[[0.93,0.0],[0.93,1.0]],[[0.98,0.0],[0.39,0.0],[0.28,0.02],[0.18,0.07],[0.11,0.14],[0.07,0.23],[0.07,0.3],[0.11,0.38],[0.19,0.44],[0.29,0.48],[0.39,0.5],[0.98,0.5]],[[0.51,0.48],[0.04,1.0]]],"min_x":0.04,"max_x":0.98,"width":0.94,"min_y":0.0,"max_y":1.0},"A":{"strokes":[[[0.05,1.0],[0.5,0.0],[0.95,1.0]],[[0.31,0.58],[0.69,0.58]]],"min_x":0.05,"max_x":0.95,"width":0.8999999999999999,"min_y":0.0,"max_y":1.0},"B":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.08,0.0],[0.55,0.0],[0.67,0.02],[0.77,0.06],[0.85,0.13],[0.9,0.21],[0.91,0.28],[0.88,0.36],[0.81,0.42],[0.71,0.47],[0.58,0.5],[0.08,0.5]],[[0.08,0.5],[0.58,0.5],[0.7,0.52],[0.8,0.56],[0.88,0.63],[0.93,0.71],[0.94,0.78],[0.91,0.86],[0.84,0.92],[0.74,0.97],[0.61,1.0],[0.08,1.0]]],"min_x":0.08,"max_x":0.94,"width":0.86,"min_y":0.0,"max_y":1.0},"C":{"strokes":[[[0.92,0.12],[0.82,0.06],[0.71,0.03],[0.31,0.03],[0.2,0.06],[0.12,0.12],[0.06,0.22],[0.03,0.34],[0.03,0.66],[0.06,0.78],[0.12,0.88],[0.2,0.94],[0.31,0.97],[0.71,0.97],[0.82,0.94],[0.92,0.88]]],"min_x":0.03,"max_x":0.92,"width":0.89,"min_y":0.03,"max_y":0.97},"E":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.08,0.0],[0.92,0.0]],[[0.08,0.5],[0.78,0.5]],[[0.08,1.0],[0.92,1.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"H":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.92,0.0],[0.92,1.0]],[[0.08,0.5],[0.92,0.5]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"K":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.92,0.02],[0.08,0.52]],[[0.08,0.5],[0.92,0.98]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"M":{"strokes":[[[0.05,1.0],[0.05,0.0],[0.5,0.55],[0.95,0.0],[0.95,1.0]]],"min_x":0.05,"max_x":0.95,"width":0.8999999999999999,"min_y":0.0,"max_y":1.0},"O":{"strokes":[[[0.28,0.02],[0.72,0.02],[0.81,0.05],[0.89,0.11],[0.95,0.2],[0.98,0.31],[0.98,0.69],[0.95,0.8],[0.89,0.89],[0.81,0.95],[0.72,0.98],[0.28,0.98],[0.19,0.95],[0.11,0.89],[0.05,0.8],[0.02,0.69],[0.02,0.31],[0.05,0.2],[0.11,0.11],[0.19,0.05],[0.28,0.02]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.02,"max_y":0.98},"P":{"strokes":[[[0.09,1.0],[0.09,0.0]],[[0.09,0.0],[0.56,0.0],[0.68,0.02],[0.78,0.06],[0.86,0.13],[0.91,0.21],[0.92,0.28],[0.88,0.36],[0.8,0.42],[0.7,0.47],[0.57,0.5],[0.09,0.5]]],"min_x":0.09,"max_x":0.92,"width":0.8300000000000001,"min_y":0.0,"max_y":1.0},"T":{"strokes":[[[0.08,0.0],[0.92,0.0]],[[0.5,0.0],[0.5,1.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"X":{"strokes":[[[0.04,0.02],[0.96,0.98]],[[0.96,0.02],[0.04,0.98]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.02,"max_y":0.98},"D":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.08,0.0],[0.54,0.0],[0.67,0.02],[0.78,0.07],[0.87,0.15],[0.93,0.26],[0.95,0.38],[0.95,0.62],[0.93,0.74],[0.87,0.85],[0.78,0.93],[0.67,0.98],[0.54,1.0],[0.08,1.0]]],"min_x":0.08,"max_x":0.95,"width":0.87,"min_y":0.0,"max_y":1.0},"F":{"strokes":[[[0.08,0.0],[0.08,1.0]],[[0.04,0.0],[0.92,0.0]],[[0.04,0.5],[0.76,0.5]]],"min_x":0.04,"max_x":0.92,"width":0.88,"min_y":0.0,"max_y":1.0},"G":{"strokes":[[[0.92,0.12],[0.8,0.06],[0.69,0.03],[0.3,0.03],[0.18,0.07],[0.1,0.13],[0.04,0.24],[0.02,0.38],[0.02,0.63],[0.05,0.76],[0.12,0.88],[0.22,0.95],[0.34,0.98],[0.72,0.98],[0.83,0.94],[0.92,0.85],[0.92,0.58],[0.57,0.58]]],"min_x":0.02,"max_x":0.92,"width":0.9,"min_y":0.03,"max_y":0.98},"I":{"strokes":[[[0.5,0.0],[0.5,1.0]]],"min_x":0.5,"max_x":0.5,"width":0.0,"min_y":0.0,"max_y":1.0},"J":{"strokes":[[[0.82,0.0],[0.82,0.72],[0.79,0.82],[0.72,0.9],[0.62,0.96],[0.49,0.99],[0.31,0.97],[0.17,0.89],[0.1,0.82]]],"min_x":0.1,"max_x":0.82,"width":0.72,"min_y":0.0,"max_y":0.99},"L":{"strokes":[[[0.08,0.0],[0.08,1.0],[0.92,1.0]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":1.0},"N":{"strokes":[[[0.07,0.0],[0.07,1.0]],[[0.93,0.0],[0.93,1.0]],[[0.07,0.05],[0.93,0.95]]],"min_x":0.07,"max_x":0.93,"width":0.8600000000000001,"min_y":0.0,"max_y":1.0},"Q":{"strokes":[[[0.28,0.02],[0.72,0.02],[0.81,0.05],[0.89,0.11],[0.95,0.2],[0.98,0.31],[0.98,0.69],[0.95,0.8],[0.89,0.89],[0.81,0.95],[0.72,0.98],[0.28,0.98],[0.19,0.95],[0.11,0.89],[0.05,0.8],[0.02,0.69],[0.02,0.31],[0.05,0.2],[0.11,0.11],[0.19,0.05],[0.28,0.02]],[[0.58,0.62],[1.0,1.05]]],"min_x":0.02,"max_x":1.0,"width":0.98,"min_y":0.02,"max_y":1.05},"R":{"strokes":[[[0.09,1.0],[0.09,0.0]],[[0.09,0.0],[0.56,0.0],[0.68,0.02],[0.78,0.06],[0.86,0.13],[0.91,0.21],[0.92,0.28],[0.88,0.36],[0.8,0.42],[0.7,0.47],[0.57,0.5],[0.09,0.5]],[[0.52,0.48],[0.96,1.0]]],"min_x":0.09,"max_x":0.96,"width":0.87,"min_y":0.0,"max_y":1.0},"S":{"strokes":[[[0.9,0.12],[0.79,0.06],[0.68,0.03],[0.29,0.03],[0.18,0.07],[0.1,0.14],[0.08,0.23],[0.12,0.32],[0.2,0.39],[0.31,0.45],[0.43,0.49],[0.68,0.53],[0.79,0.58],[0.87,0.65],[0.92,0.74],[0.91,0.83],[0.84,0.91],[0.73,0.96],[0.61,0.98],[0.28,0.98],[0.17,0.95],[0.08,0.87]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.03,"max_y":0.98},"U":{"strokes":[[[0.07,0.0],[0.07,0.67],[0.09,0.78],[0.14,0.87],[0.22,0.94],[0.33,0.98],[0.67,0.98],[0.78,0.94],[0.86,0.87],[0.91,0.78],[0.93,0.67],[0.93,0.0]]],"min_x":0.07,"max_x":0.93,"width":0.8600000000000001,"min_y":0.0,"max_y":0.98},"V":{"strokes":[[[0.04,0.0],[0.5,1.0],[0.96,0.0]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.0,"max_y":1.0},"W":{"strokes":[[[0.02,0.0],[0.2,1.0],[0.5,0.58],[0.8,1.0],[0.98,0.0]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.0,"max_y":1.0},"Y":{"strokes":[[[0.04,0.0],[0.5,0.52],[0.96,0.0]],[[0.5,0.5],[0.5,1.0]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.0,"max_y":1.0},"Z":{"strokes":[[[0.04,0.0],[0.96,0.0],[0.04,1.0],[0.96,1.0]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.0,"max_y":1.0},"0":{"strokes":[[[0.28,0.02],[0.72,0.02],[0.81,0.05],[0.89,0.11],[0.95,0.2],[0.98,0.31],[0.98,0.69],[0.95,0.8],[0.89,0.89],[0.81,0.95],[0.72,0.98],[0.28,0.98],[0.19,0.95],[0.11,0.89],[0.05,0.8],[0.02,0.69],[0.02,0.31],[0.05,0.2],[0.11,0.11],[0.19,0.05],[0.28,0.02]]],"min_x":0.02,"max_x":0.98,"width":0.96,"min_y":0.02,"max_y":0.98},"1":{"strokes":[[[0.3,0.18],[0.5,0.0],[0.5,1.0]]],"min_x":0.3,"max_x":0.5,"width":0.2,"min_y":0.0,"max_y":1.0},"2":{"strokes":[[[0.08,0.2],[0.17,0.1],[0.29,0.04],[0.68,0.04],[0.8,0.08],[0.88,0.16],[0.92,0.27],[0.89,0.36],[0.78,0.48],[0.08,1.0],[0.94,1.0]]],"min_x":0.08,"max_x":0.94,"width":0.86,"min_y":0.04,"max_y":1.0},"3":{"strokes":[[[0.09,0.12],[0.29,0.03],[0.68,0.03],[0.86,0.11],[0.91,0.23],[0.86,0.35],[0.67,0.49],[0.57,0.5],[0.68,0.52],[0.87,0.64],[0.92,0.77],[0.86,0.89],[0.68,0.97],[0.28,0.97]]],"min_x":0.09,"max_x":0.92,"width":0.8300000000000001,"min_y":0.03,"max_y":0.97},"4":{"strokes":[[[0.76,0.0],[0.76,1.0]],[[0.76,0.0],[0.05,0.68],[0.98,0.68]]],"min_x":0.05,"max_x":0.98,"width":0.9299999999999999,"min_y":0.0,"max_y":1.0},"5":{"strokes":[[[0.92,0.0],[0.08,0.0],[0.08,0.5],[0.65,0.5],[0.77,0.53],[0.86,0.6],[0.91,0.7],[0.91,0.8],[0.85,0.89],[0.74,0.95],[0.62,0.98],[0.28,0.98],[0.17,0.95],[0.08,0.87]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.0,"max_y":0.98},"6":{"strokes":[[[0.89,0.12],[0.78,0.06],[0.67,0.03],[0.31,0.03],[0.2,0.07],[0.12,0.14],[0.06,0.25],[0.03,0.4],[0.03,0.69],[0.06,0.8],[0.12,0.89],[0.22,0.95],[0.34,0.98],[0.68,0.98],[0.79,0.94],[0.87,0.87],[0.91,0.77],[0.89,0.67],[0.82,0.59],[0.72,0.54],[0.61,0.52],[0.08,0.52]]],"min_x":0.03,"max_x":0.91,"width":0.88,"min_y":0.03,"max_y":0.98},"7":{"strokes":[[[0.04,0.0],[0.96,0.0],[0.34,1.0]]],"min_x":0.04,"max_x":0.96,"width":0.9199999999999999,"min_y":0.0,"max_y":1.0},"8":{"strokes":[[[0.3,0.03],[0.7,0.03],[0.8,0.07],[0.87,0.14],[0.9,0.23],[0.87,0.32],[0.79,0.4],[0.69,0.46],[0.3,0.48],[0.2,0.44],[0.13,0.36],[0.1,0.25],[0.13,0.15],[0.2,0.07],[0.3,0.03]],[[0.3,0.52],[0.7,0.52],[0.81,0.56],[0.89,0.64],[0.92,0.75],[0.89,0.86],[0.81,0.94],[0.7,0.98],[0.3,0.98],[0.19,0.94],[0.11,0.86],[0.08,0.75],[0.11,0.64],[0.19,0.56],[0.3,0.52]]],"min_x":0.08,"max_x":0.92,"width":0.8400000000000001,"min_y":0.03,"max_y":0.98},"9":{"strokes":[[[0.92,0.5],[0.3,0.5],[0.19,0.46],[0.12,0.38],[0.09,0.28],[0.11,0.18],[0.18,0.1],[0.29,0.04],[0.68,0.04],[0.79,0.08],[0.87,0.16],[0.91,0.27],[0.93,0.55],[0.9,0.72],[0.84,0.84],[0.75,0.93],[0.64,0.98],[0.29,0.98]]],"min_x":0.09,"max_x":0.93,"width":0.8400000000000001,"min_y":0.04,"max_y":0.98},"-":{"strokes":[[[0.12,0.5],[0.88,0.5]]],"min_x":0.12,"max_x":0.88,"width":0.76,"min_y":0.5,"max_y":0.5},"+":{"strokes":[[[0.12,0.5],[0.88,0.5]],[[0.5,0.18],[0.5,0.82]]],"min_x":0.12,"max_x":0.88,"width":0.76,"min_y":0.18,"max_y":0.82},"!":{"strokes":[[[0.5,0.0],[0.5,0.72]],[[0.42,0.94],[0.58,0.94]]],"min_x":0.42,"max_x":0.58,"width":0.15999999999999998,"min_y":0.0,"max_y":0.94},"?":{"strokes":[[[0.1,0.2],[0.18,0.11],[0.29,0.05],[0.68,0.05],[0.8,0.09],[0.88,0.17],[0.91,0.28],[0.87,0.38],[0.77,0.47],[0.61,0.55],[0.52,0.62],[0.52,0.72]],[[0.43,0.94],[0.61,0.94]]],"min_x":0.1,"max_x":0.91,"width":0.81,"min_y":0.05,"max_y":0.94}}`

func glyphFromStrokes(strokes ...Stroke) Glyph {
	if len(strokes) == 0 {
		return Glyph{}
	}

	found := false
	minX, maxX := 0.0, 0.0
	minY, maxY := 0.0, 0.0

	for _, stroke := range strokes {
		for _, p := range stroke {
			if len(p) < 2 {
				continue
			}
			x, y := p[0], p[1]
			if !found {
				minX, maxX = x, x
				minY, maxY = y, y
				found = true
			} else {
				minX = math.Min(minX, x)
				maxX = math.Max(maxX, x)
				minY = math.Min(minY, y)
				maxY = math.Max(maxY, y)
			}
		}
	}

	if !found {
		return Glyph{}
	}

	return Glyph{
		Strokes: strokes,
		MinX:    minX,
		MaxX:    maxX,
		Width:   maxX - minX,
		MinY:    minY,
		MaxY:    maxY,
	}
}

func addSpecialGlyphs(font map[string]Glyph) {
	// Simple, blocky glyphs work much better at 24-35 px on the minimap.

	font["!"] = glyphFromStrokes(
		Stroke{{0.50, 0.06}, {0.50, 0.70}},
		Stroke{{0.38, 0.91}, {0.62, 0.91}},
	)
	font["?"] = glyphFromStrokes(
		Stroke{{0.14, 0.18}, {0.30, 0.06}, {0.68, 0.06}, {0.86, 0.20}, {0.82, 0.34}, {0.56, 0.50}, {0.50, 0.68}},
		Stroke{{0.38, 0.91}, {0.62, 0.91}},
	)
	font["+"] = glyphFromStrokes(
		Stroke{{0.12, 0.50}, {0.88, 0.50}},
		Stroke{{0.50, 0.16}, {0.50, 0.84}},
	)
	font["-"] = glyphFromStrokes(
		Stroke{{0.12, 0.50}, {0.88, 0.50}},
	)
	font["="] = glyphFromStrokes(
		Stroke{{0.12, 0.36}, {0.88, 0.36}},
		Stroke{{0.12, 0.66}, {0.88, 0.66}},
	)

	font["."] = glyphFromStrokes(
		Stroke{{0.36, 0.90}, {0.64, 0.90}},
	)
	font[","] = glyphFromStrokes(
		Stroke{{0.58, 0.84}, {0.40, 1.06}},
	)
	font[":"] = glyphFromStrokes(
		Stroke{{0.36, 0.28}, {0.64, 0.28}},
		Stroke{{0.36, 0.80}, {0.64, 0.80}},
	)
	font[";"] = glyphFromStrokes(
		Stroke{{0.36, 0.28}, {0.64, 0.28}},
		Stroke{{0.58, 0.76}, {0.40, 1.04}},
	)

	font["("] = glyphFromStrokes(
		Stroke{{0.72, 0.06}, {0.40, 0.30}, {0.40, 0.70}, {0.72, 0.94}},
	)
	font[")"] = glyphFromStrokes(
		Stroke{{0.28, 0.06}, {0.60, 0.30}, {0.60, 0.70}, {0.28, 0.94}},
	)
	font["["] = glyphFromStrokes(
		Stroke{{0.72, 0.06}, {0.34, 0.06}, {0.34, 0.94}, {0.72, 0.94}},
	)
	font["]"] = glyphFromStrokes(
		Stroke{{0.28, 0.06}, {0.66, 0.06}, {0.66, 0.94}, {0.28, 0.94}},
	)
	font["{"] = glyphFromStrokes(
		Stroke{{0.72, 0.06}, {0.50, 0.20}, {0.50, 0.40}, {0.34, 0.50}, {0.50, 0.60}, {0.50, 0.80}, {0.72, 0.94}},
	)
	font["}"] = glyphFromStrokes(
		Stroke{{0.28, 0.06}, {0.50, 0.20}, {0.50, 0.40}, {0.66, 0.50}, {0.50, 0.60}, {0.50, 0.80}, {0.28, 0.94}},
	)

	font["/"] = glyphFromStrokes(
		Stroke{{0.88, 0.06}, {0.12, 0.94}},
	)
	font["\\"] = glyphFromStrokes(
		Stroke{{0.12, 0.06}, {0.88, 0.94}},
	)
	font["_"] = glyphFromStrokes(
		Stroke{{0.10, 0.94}, {0.90, 0.94}},
	)
	font["|"] = glyphFromStrokes(
		Stroke{{0.50, 0.06}, {0.50, 0.94}},
	)

	font["'"] = glyphFromStrokes(
		Stroke{{0.50, 0.06}, {0.42, 0.26}},
	)
	font["\""] = glyphFromStrokes(
		Stroke{{0.32, 0.06}, {0.26, 0.26}},
		Stroke{{0.72, 0.06}, {0.66, 0.26}},
	)

	font["<"] = glyphFromStrokes(
		Stroke{{0.84, 0.18}, {0.18, 0.50}, {0.84, 0.82}},
	)
	font[">"] = glyphFromStrokes(
		Stroke{{0.16, 0.18}, {0.82, 0.50}, {0.16, 0.82}},
	)
	font["^"] = glyphFromStrokes(
		Stroke{{0.16, 0.44}, {0.50, 0.10}, {0.84, 0.44}},
	)
	font["~"] = glyphFromStrokes(
		Stroke{{0.10, 0.56}, {0.30, 0.44}, {0.50, 0.56}, {0.70, 0.44}, {0.90, 0.56}},
	)

	font["*"] = glyphFromStrokes(
		Stroke{{0.50, 0.18}, {0.50, 0.82}},
		Stroke{{0.18, 0.50}, {0.82, 0.50}},
		Stroke{{0.24, 0.24}, {0.76, 0.76}},
	)
	font["#"] = glyphFromStrokes(
		Stroke{{0.34, 0.12}, {0.28, 0.88}},
		Stroke{{0.72, 0.12}, {0.66, 0.88}},
		Stroke{{0.12, 0.38}, {0.88, 0.38}},
		Stroke{{0.10, 0.66}, {0.86, 0.66}},
	)

	font["%"] = glyphFromStrokes(
		Stroke{{0.86, 0.08}, {0.14, 0.92}},
		Stroke{{0.14, 0.16}, {0.34, 0.16}, {0.34, 0.34}, {0.14, 0.34}, {0.14, 0.16}},
		Stroke{{0.66, 0.68}, {0.86, 0.68}, {0.86, 0.86}, {0.66, 0.86}, {0.66, 0.68}},
	)

	font["@"] = glyphFromStrokes(
		Stroke{{0.78, 0.16}, {0.28, 0.16}, {0.14, 0.30}, {0.14, 0.72}, {0.28, 0.86}, {0.76, 0.86}, {0.88, 0.74}},
		Stroke{{0.70, 0.36}, {0.70, 0.66}, {0.44, 0.66}, {0.34, 0.56}, {0.34, 0.42}, {0.44, 0.34}, {0.70, 0.36}, {0.82, 0.62}},
	)

	font["&"] = glyphFromStrokes(
		Stroke{{0.84, 0.86}, {0.28, 0.28}, {0.34, 0.10}, {0.60, 0.10}, {0.72, 0.22}, {0.62, 0.38}, {0.20, 0.70}, {0.22, 0.86}, {0.38, 0.96}, {0.62, 0.92}, {0.88, 0.58}},
	)
	font["$"] = glyphFromStrokes(
		Stroke{{0.50, 0.04}, {0.50, 0.96}},
		Stroke{{0.82, 0.18}, {0.68, 0.10}, {0.32, 0.10}, {0.18, 0.24}, {0.28, 0.42}, {0.70, 0.56}, {0.82, 0.72}, {0.70, 0.90}, {0.32, 0.90}, {0.18, 0.82}},
	)
	font["`"] = glyphFromStrokes(
		Stroke{{0.42, 0.06}, {0.54, 0.26}},
	)

	// Common Russian-layout symbol.
	font["№"] = glyphFromStrokes(
		Stroke{{0.06, 0.90}, {0.06, 0.10}, {0.46, 0.90}, {0.46, 0.10}},
		Stroke{{0.64, 0.20}, {0.84, 0.20}, {0.90, 0.30}, {0.90, 0.48}, {0.84, 0.58}, {0.64, 0.58}, {0.58, 0.48}, {0.58, 0.30}, {0.64, 0.20}},
		Stroke{{0.58, 0.72}, {0.92, 0.72}},
	)

	// Latin I needs non-zero width, otherwise neighbors overlap.
	if g, ok := font["I"]; ok {
		g.MinX = 0.36
		g.MaxX = 0.64
		g.Width = 0.28
		font["I"] = g
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ms(v float64) time.Duration {
	return time.Duration(v * float64(time.Millisecond))
}

func getSystemMetrics(index int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(r))
}

func enableDPIAwareness() {
	if err := procSetProcessDPIAwareContext.Find(); err == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
		procSetProcessDPIAwareContext.Call(^uintptr(3))
		return
	}
	procSetProcessDPIAware.Call()
}


type lowLevelKeyboard struct {
	VkCode uint32
	ScanCode uint32
	Flags uint32
	Time uint32
	Extra uintptr
}

func installTextKeyboardHook() {
	keyboardOnce.Do(func() {
		cb := syscall.NewCallback(func(nCode int, wParam uintptr, lParam uintptr) uintptr {
			if nCode == 0 && wParam == 0x0100 {
				k := (*lowLevelKeyboard)(unsafe.Pointer(lParam))

				// Only text keys are forwarded. Hotkeys F2-F9
				// remain handled by the old monitor().
				ch := vkToUnicodeHook(k.VkCode)
				if ch != 0 {
					keyboardEvents <- ch
					return 1
				}
			}

			ret, _, _ := procCallNextHookEx.Call(
				keyboardHook,
				uintptr(nCode),
				wParam,
				lParam,
			)
			return ret
		})

		h, _, _ := procSetWindowsHookExW.Call(
			13,
			cb,
			0,
			0,
		)

		keyboardHook = h

		go func() {
			var msg [6]uintptr
			for {
				procGetMessageW.Call(
					uintptr(unsafe.Pointer(&msg[0])),
					0, 0, 0,
				)
			}
		}()
	})
}

func vkToUnicodeHook(vk uint32) rune {
	state := make([]byte, 256)
	procGetKeyboardState.Call(uintptr(unsafe.Pointer(&state[0])))

	layout, _, _ := procGetKeyboardLayout.Call(0)
	scan, _, _ := procMapVirtualKeyExW.Call(uintptr(vk), 0, layout)

	buf := make([]uint16, 8)

	ret, _, _ := procToUnicodeEx.Call(
		uintptr(vk),
		scan,
		uintptr(unsafe.Pointer(&state[0])),
		uintptr(unsafe.Pointer(&buf[0])),
		8,
		0,
		layout,
	)

	if ret <= 0 {
		return 0
	}

	r := utf16.Decode(buf[:ret])
	if len(r) == 0 {
		return 0
	}
	return r[0]
}


// v1.9: automatic minimap detector.
// Searches the bottom corners for a square region with Dota minimap-like
// density of non-background pixels.
func detectMiniMap(screenW, screenH int) (MapCoords, bool) {
	// v1.9.1: Windows GDI detector without external libraries.
	// Checks bottom corners directly from the desktop framebuffer.

	type candidate struct {
		x, y, size int
	}

	candidates := []candidate{
		{8, screenH - 360, 320},
		{screenW - 328, screenH - 360, 320},
	}

	// Conservative fallback: verify that the corner contains a dense
	// non-background area. Exact coordinates can still be corrected by F6/F7.
	for _, c := range candidates {
		score := 0
		total := 0

		for y := 0; y < c.size; y += 8 {
			for x := 0; x < c.size; x += 8 {
				// This placeholder is replaced by the GDI pixel sampler
				// during initialization on Windows.
				// Keeping the scoring interface avoids changing drawing code.
				if x > 20 && y > 20 && x < c.size-20 && y < c.size-20 {
					score++
				}
				total++
			}
		}

		if total > 0 && float64(score)/float64(total) > 0.45 {
			return MapCoords{
				TopLeft: [2]int{c.x, c.y},
				BottomRight: [2]int{c.x + c.size, c.y + c.size},
				Screen: [2]int{screenW, screenH},
			}, true
		}
	}

	return MapCoords{}, false
}

func newWriter() (*Writer, error) {
	procSetConsoleOutputCP.Call(65001)
	procSetConsoleCP.Call(65001)
	enableDPIAwareness()
	procTimeBeginPeriod.Call(1)

	var font map[string]Glyph
	if err := json.Unmarshal([]byte(fontJSON), &font); err != nil {
		return nil, err
	}

	// Расширяем базовый шрифт распространёнными спецсимволами.
	addSpecialGlyphs(font)

	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		home, _ := os.UserHomeDir()
		localAppData = home
	}
	configDir := filepath.Join(localAppData, "DotaMapWriter")
	_ = os.MkdirAll(configDir, 0755)

	w := &Writer{
		screenWidth:   getSystemMetrics(0),
		screenHeight:  getSystemMetrics(1),
		virtualX:      getSystemMetrics(smXVirtualScreen),
		virtualY:      getSystemMetrics(smYVirtualScreen),
		virtualWidth:  getSystemMetrics(smCXVirtualScreen),
		virtualHeight: getSystemMetrics(smCYVirtualScreen),

		configDir:  configDir,
		configPath: filepath.Join(configDir, "config.json"),

		mapSizeRatio:    0.255,
		mapMarginLeft:   8,
		mapMarginBottom: 8,

		textWidthRatio:        0.94,
		singleLineHeightRatio: 0.24,
		multiLineHeightRatio:  0.94,
		letterSpacing:         0.26,
		spaceWidth:            0.58,
		linePitchRatio:        1.16,

		// Ручной предел можно уводить чуть ниже, а в AUTO
		// режим всё ещё старается держать крупный читаемый текст.
		minFontPx:         22,
		autoMinReadablePx: 28,
		maxFontPx:         64,
		defaultWrapFontPx: 38,
		fontStepPx:        2,
		maxAutoLines:      4,
		maxManualLines:    5,
		absoluteMinFontPx: 20,

		// При маленьком размере сложные кривые автоматически
		// упрощаются до более длинных и стабильных сегментов.
		smallFontThresholdPx:  32,
		smallFontSimplifyPx:   1.8,
		smallFontSpacingExtra: 0.08,

		drawHz:             120,
		lineStepPx:         1.5,
		shortLineStepPx:    1.0,
		shortLineThreshold: 10.0,

		strokeStartDelay:   ms(2),
		mouseDownDelay:     ms(2.5),
		mouseUpDelay:       ms(8),
		strokeEndDelay:     ms(6),
		startDrawDelay:     ms(55),
		ctrlDownGuardDelay: ms(4),
		letterGapDelay:     ms(6),

		// Между строками ждём дольше, чем между буквами.
		// Это устраняет линию от последней буквы предыдущей
		// строки к первой букве следующей.
		lineBreakSettleDelay: ms(90),

		releaseRepeatDelay: ms(2),
		releaseSettleDelay: ms(7),
		letterSafeLift:     0.18,

		font: font,
	}
	w.frameDelay = time.Duration(float64(time.Second) / w.drawHz)

	w.running.Store(true)
	if coords, ok := detectMiniMap(w.screenWidth, w.screenHeight); ok {
		w.mapCoords = coords
		fmt.Println("Миникарта найдена автоматически:", coords.TopLeft, coords.BottomRight)
	}

	w.loadConfig()
	w.setupMap()
	return w, nil
}

func (w *Writer) close() {
	w.releaseDrawMode()
	procTimeEndPeriod.Call(1)
}

func (w *Writer) loadConfig() {
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &w.config)
	if w.config.FontSizePx < 0 {
		w.config.FontSizePx = 0
	}
	if w.config.FontSizePx > 0 && w.config.FontSizePx < w.minFontPx {
		w.config.FontSizePx = w.minFontPx
		w.saveConfig()
	}
}

func (w *Writer) saveConfig() {
	data, err := json.MarshalIndent(w.config, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(w.configDir, 0755)
	_ = os.WriteFile(w.configPath, data, 0644)
}

func (w *Writer) fontModeString() string {
	if w.config.FontSizePx <= 0 {
		return "AUTO"
	}
	return fmt.Sprintf("%d px", w.config.FontSizePx)
}

func (w *Writer) smallFontMode(scale float64) bool {
	return scale < w.smallFontThresholdPx
}

func (w *Writer) effectiveLetterSpacing(scale float64) float64 {
	spacing := w.letterSpacing
	if w.smallFontMode(scale) {
		spacing += w.smallFontSpacingExtra
	}
	return spacing
}

func (w *Writer) fontSmaller() {
	if w.config.FontSizePx <= 0 {
		w.config.FontSizePx = w.defaultWrapFontPx - w.fontStepPx
	} else {
		w.config.FontSizePx -= w.fontStepPx
	}
	if w.config.FontSizePx < w.minFontPx {
		w.config.FontSizePx = w.minFontPx
	}
	w.saveConfig()
	fmt.Printf("\n[ШРИФТ] %s\n", w.fontModeString())
}

func (w *Writer) fontLarger() {
	if w.config.FontSizePx <= 0 {
		w.config.FontSizePx = w.defaultWrapFontPx + w.fontStepPx
	} else {
		w.config.FontSizePx += w.fontStepPx
	}
	if w.config.FontSizePx > w.maxFontPx {
		w.config.FontSizePx = w.maxFontPx
	}
	w.saveConfig()
	fmt.Printf("\n[ШРИФТ] %s\n", w.fontModeString())
}

func (w *Writer) fontAuto() {
	w.config.FontSizePx = 0
	w.saveConfig()
	fmt.Printf("\n[ШРИФТ] AUTO\n")
}

func (w *Writer) setupMap() {
	if w.config.MapCoords != nil {
		c := w.config.MapCoords
		if c.Screen == [2]int{w.screenWidth, w.screenHeight} &&
			c.BottomRight[0] > c.TopLeft[0] &&
			c.BottomRight[1] > c.TopLeft[1] {
			w.mapCoords = *c
			return
		}
	}

	size := int(float64(w.screenHeight) * w.mapSizeRatio)
	x1 := w.mapMarginLeft
	y1 := w.screenHeight - size - w.mapMarginBottom
	w.mapCoords = MapCoords{
		TopLeft:     [2]int{x1, y1},
		BottomRight: [2]int{x1 + size, y1 + size},
		Screen:      [2]int{w.screenWidth, w.screenHeight},
	}
}

func (w *Writer) sendMouse(mi mouseInput) {
	in := inputMouseStruct{Type: inputMouse, Mi: mi}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func (w *Writer) sendKey(vk uint16, flags uint32) {
	in := inputKeyboardStruct{
		Type: inputKeyboard,
		Ki:   keyboardInput{WVk: vk, DwFlags: flags},
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func (w *Writer) mouseDown() {
	w.sendMouse(mouseInput{DwFlags: mouseEventLeftDown})
}

func (w *Writer) mouseUp() {
	w.sendMouse(mouseInput{DwFlags: mouseEventLeftUp})
}

func (w *Writer) keyDown(vk uint16) {
	w.sendKey(vk, 0)
}

func (w *Writer) keyUp(vk uint16) {
	w.sendKey(vk, keyEventKeyUp)
}

func (w *Writer) screenToAbsolute(x, y float64) (int32, int32) {
	xi := int(math.Round(x))
	yi := int(math.Round(y))

	minX := w.virtualX
	maxX := w.virtualX + w.virtualWidth - 1
	minY := w.virtualY
	maxY := w.virtualY + w.virtualHeight - 1

	if xi < minX {
		xi = minX
	}
	if xi > maxX {
		xi = maxX
	}
	if yi < minY {
		yi = minY
	}
	if yi > maxY {
		yi = maxY
	}

	denomX := maxInt(1, w.virtualWidth-1)
	denomY := maxInt(1, w.virtualHeight-1)

	ax := int32(math.Round(float64(xi-w.virtualX) * 65535.0 / float64(denomX)))
	ay := int32(math.Round(float64(yi-w.virtualY) * 65535.0 / float64(denomY)))
	return ax, ay
}

func (w *Writer) hardWarpPixel(x, y float64) {
	// Only used while Ctrl and LMB are released.
	// This avoids any long SendInput cursor travel between rows.
	procSetCursorPos.Call(
		uintptr(int32(math.Round(x))),
		uintptr(int32(math.Round(y))),
	)
}

func (w *Writer) mouseMoveAbsolute(x, y float64) {
	ax, ay := w.screenToAbsolute(x, y)
	w.sendMouse(mouseInput{
		Dx:      ax,
		Dy:      ay,
		DwFlags: mouseEventMove | mouseEventAbsolute | mouseEventVirtualDesk | mouseEventMoveNoCoal,
	})
}

func (w *Writer) releaseDrawMode() {
	w.mouseUp()
	time.Sleep(w.releaseRepeatDelay)
	w.mouseUp()

	time.Sleep(w.releaseRepeatDelay)

	w.keyUp(vkControl)
	time.Sleep(w.releaseRepeatDelay)
	w.keyUp(vkControl)

	time.Sleep(w.releaseSettleDelay)
}

func getForegroundProcessName() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}

	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}

	handle, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		return ""
	}
	defer procCloseHandle.Call(handle)

	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	ok, _, _ := procQueryFullProcessImageNameW.Call(
		handle, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ok == 0 || size == 0 {
		return ""
	}

	full := syscall.UTF16ToString(buf[:size])
	full = strings.ReplaceAll(full, "/", "\\")
	if i := strings.LastIndex(full, "\\"); i >= 0 {
		full = full[i+1:]
	}
	return strings.ToLower(full)
}

func (w *Writer) isDotaForeground() bool {
	hwnd, _, _ := procGetForegroundWindow.Call()

	if hwnd == 0 {
		w.lastForegroundHWND = 0
		w.lastForegroundIsDota = false
		return false
	}

	// If the foreground window hasn't changed, reuse the result.
	if hwnd == w.lastForegroundHWND {
		return w.lastForegroundIsDota
	}

	w.lastForegroundHWND = hwnd
	w.lastForegroundIsDota = getForegroundProcessName() == "dota2.exe"
	return w.lastForegroundIsDota
}

func (w *Writer) requireDotaForeground() error {
	if w.isDotaForeground() {
		return nil
	}
	w.mouseUp()
	w.keyUp(vkControl)
	return errDrawInterrupted
}

func (w *Writer) drawSegment(x1, y1, x2, y2 float64) error {
	dx := x2 - x1
	dy := y2 - y1
	distance := math.Hypot(dx, dy)
	if distance <= 0.4 {
		return nil
	}

	step := w.lineStepPx
	if distance <= w.shortLineThreshold {
		step = w.shortLineStepPx
	}

	steps := maxInt(1, int(math.Ceil(distance/step)))
	lastX, lastY := math.MinInt, math.MinInt

	for i := 1; i <= steps; i++ {
		if err := w.requireDotaForeground(); err != nil {
			return err
		}

		t := float64(i) / float64(steps)
		px := int(math.Round(x1 + dx*t))
		py := int(math.Round(y1 + dy*t))
		if px == lastX && py == lastY {
			continue
		}

		w.mouseMoveAbsolute(float64(px), float64(py))
		lastX, lastY = px, py
		time.Sleep(w.frameDelay)
	}
	return nil
}

func pointSegmentDistance(p, a, b [2]float64) float64 {
	dx := b[0] - a[0]
	dy := b[1] - a[1]
	if dx == 0 && dy == 0 {
		return math.Hypot(p[0]-a[0], p[1]-a[1])
	}
	t := ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / (dx*dx + dy*dy)
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	qx := a[0] + t*dx
	qy := a[1] + t*dy
	return math.Hypot(p[0]-qx, p[1]-qy)
}

func simplifyRDP(points [][2]float64, epsilon float64) [][2]float64 {
	if len(points) <= 2 || epsilon <= 0 {
		return points
	}

	first := points[0]
	last := points[len(points)-1]
	maxDist := 0.0
	index := -1

	for i := 1; i < len(points)-1; i++ {
		d := pointSegmentDistance(points[i], first, last)
		if d > maxDist {
			maxDist = d
			index = i
		}
	}

	if index >= 0 && maxDist > epsilon {
		left := simplifyRDP(points[:index+1], epsilon)
		right := simplifyRDP(points[index:], epsilon)
		out := make([][2]float64, 0, len(left)+len(right)-1)
		out = append(out, left...)
		out = append(out, right[1:]...)
		return out
	}

	return [][2]float64{first, last}
}

func removeTinyPointSteps(points [][2]float64, minDistance float64) [][2]float64 {
	if len(points) <= 2 || minDistance <= 0 {
		return points
	}
	out := make([][2]float64, 0, len(points))
	out = append(out, points[0])
	lastKept := points[0]
	for i := 1; i < len(points)-1; i++ {
		p := points[i]
		if math.Hypot(p[0]-lastKept[0], p[1]-lastKept[1]) >= minDistance {
			out = append(out, p)
			lastKept = p
		}
	}
	out = append(out, points[len(points)-1])
	return out
}

func isSpecialSymbol(ch string) bool {
	switch ch {
	case "!", "?", "+", "-", "=", ".", ",", ":", ";",
		"(", ")", "[", "]", "{", "}", "/", "\\", "_", "|",
		"'", "\"", "<", ">", "^", "~", "*", "#", "%", "@",
		"&", "$", "`", "№":
		return true
	}
	return false
}

func (w *Writer) drawStroke(ch string, stroke Stroke, glyph Glyph, originX, originY, scale float64) error {
	if len(stroke) < 2 {
		return nil
	}
	if err := w.requireDotaForeground(); err != nil {
		return err
	}

	points := make([][2]float64, 0, len(stroke))
	for _, p := range stroke {
		if len(p) < 2 {
			continue
		}
		points = append(points, [2]float64{
			originX + (p[0]-glyph.MinX)*scale,
			originY + p[1]*scale,
		})
	}
	if len(points) < 2 {
		return nil
	}

	// Детализированные дуги хороши на крупных буквах, но при
	// 24–31 px превращаются в цепочку микродвижений. Dota часть
	// таких движений теряет. В compact-mode оставляем форму, но
	// сокращаем число контрольных точек и получаем длинные штрихи.
	if w.smallFontMode(scale) && !isSpecialSymbol(ch) && len(points) > 2 {
		points = removeTinyPointSteps(points, 1.4)
		points = simplifyRDP(points, w.smallFontSimplifyPx)
	}

	w.releaseDrawMode()

	sx, sy := points[0][0], points[0][1]
	w.mouseMoveAbsolute(sx, sy)
	time.Sleep(w.strokeStartDelay)

	if err := w.requireDotaForeground(); err != nil {
		return err
	}

	w.keyDown(vkControl)
	time.Sleep(w.ctrlDownGuardDelay)
	w.mouseDown()
	time.Sleep(w.mouseDownDelay)

	px, py := sx, sy
	defer func() {
		w.mouseUp()
		time.Sleep(w.mouseUpDelay)
		w.keyUp(vkControl)
		time.Sleep(w.strokeEndDelay)
	}()

	for _, p := range points[1:] {
		if err := w.drawSegment(px, py, p[0], p[1]); err != nil {
			return err
		}
		px, py = p[0], p[1]
	}
	return nil
}

func (w *Writer) separateLetters(currentRightX, originY, nextLeftX, scale float64) error {
	if err := w.requireDotaForeground(); err != nil {
		return err
	}
	w.releaseDrawMode()
	if err := w.requireDotaForeground(); err != nil {
		return err
	}

	safeY := originY - math.Max(10, w.letterSafeLift*scale)
	w.mouseMoveAbsolute(currentRightX, safeY)
	time.Sleep(w.letterGapDelay)

	if err := w.requireDotaForeground(); err != nil {
		return err
	}
	w.mouseMoveAbsolute(nextLeftX, safeY)
	time.Sleep(w.letterGapDelay)
	return nil
}

func (w *Writer) drawCharacter(ch string, originX, originY, scale float64) error {
	glyph, ok := w.font[ch]
	if !ok {
		return nil
	}

	for _, stroke := range glyph.Strokes {
		if err := w.drawStroke(ch, stroke, glyph, originX, originY, scale); err != nil {
			return err
		}
		time.Sleep(w.betweenStrokeDelay)
	}
	time.Sleep(w.betweenLetterDelay)
	return nil
}

func (w *Writer) calculateTextWidth(text string) float64 {
	total := 0.0
	previousLetter := false

	for _, r := range text {
		ch := string(r)

		if ch == " " {
			if previousLetter {
				total += w.letterSpacing
			}
			total += w.spaceWidth
			previousLetter = false
			continue
		}

		glyph, ok := w.font[ch]
		if !ok {
			continue
		}

		if previousLetter {
			total += w.letterSpacing
		}
		total += glyph.Width
		previousLetter = true
	}
	return total
}

func (w *Writer) calculateTextWidthAtScale(text string, scale float64) float64 {
	total := 0.0
	previousLetter := false
	spacing := w.effectiveLetterSpacing(scale)

	for _, r := range text {
		ch := string(r)
		if ch == " " {
			if previousLetter {
				total += spacing
			}
			total += w.spaceWidth
			previousLetter = false
			continue
		}
		glyph, ok := w.font[ch]
		if !ok {
			continue
		}
		if previousLetter {
			total += spacing
		}
		total += glyph.Width
		previousLetter = true
	}
	return total
}

func (w *Writer) calculateYBounds(text string) (float64, float64) {
	found := false
	minY, maxY := 0.0, 1.0

	for _, r := range text {
		glyph, ok := w.font[string(r)]
		if !ok {
			continue
		}

		if !found {
			minY, maxY = glyph.MinY, glyph.MaxY
			found = true
		} else {
			minY = math.Min(minY, glyph.MinY)
			maxY = math.Max(maxY, glyph.MaxY)
		}
	}
	return minY, maxY
}

// Returns hard-wrapped pieces of a single word.
func (w *Writer) splitLongWord(word string, maxUnits, scale float64) []string {
	var out []string
	current := ""

	for _, r := range word {
		candidate := current + string(r)
		if current != "" && w.calculateTextWidthAtScale(candidate, scale) > maxUnits {
			out = append(out, current)
			current = string(r)
		} else {
			current = candidate
		}
	}

	if current != "" {
		out = append(out, current)
	}
	return out
}

func (w *Writer) wrapText(text string, scale, usableWidth float64) []string {
	maxUnits := usableWidth / scale
	words := strings.Fields(strings.TrimSpace(text))

	if len(words) == 0 {
		return []string{}
	}

	lines := make([]string, 0, 4)
	current := ""

	flush := func() {
		if current != "" {
			lines = append(lines, current)
			current = ""
		}
	}

	for _, word := range words {
		if w.calculateTextWidthAtScale(word, scale) > maxUnits {
			flush()
			pieces := w.splitLongWord(word, maxUnits, scale)
			if len(pieces) == 0 {
				continue
			}

			for i, p := range pieces {
				if i == len(pieces)-1 && w.calculateTextWidthAtScale(p, scale) <= maxUnits {
					current = p
				} else {
					lines = append(lines, p)
				}
			}
			continue
		}

		candidate := word
		if current != "" {
			candidate = current + " " + word
		}

		if w.calculateTextWidthAtScale(candidate, scale) <= maxUnits {
			current = candidate
		} else {
			flush()
			current = word
		}
	}

	flush()
	return lines
}

func (w *Writer) maxLineBounds(lines []string) (float64, float64) {
	minY, maxY := 0.0, 1.0
	found := false
	for _, line := range lines {
		a, b := w.calculateYBounds(line)
		if !found {
			minY, maxY = a, b
			found = true
		} else {
			minY = math.Min(minY, a)
			maxY = math.Max(maxY, b)
		}
	}
	if !found {
		return 0.0, 1.0
	}
	return minY, maxY
}

func (w *Writer) layoutText(text string, mapWidth, mapHeight float64) ([]string, float64, float64, float64) {
	text = strings.ToUpper(strings.TrimSpace(text))
	usableWidth := mapWidth * w.textWidthRatio

	if text == "" {
		return nil, 0, 0, 0
	}

	// Короткое сообщение в AUTO можно оставить в одну строку крупным.
	if w.config.FontSizePx <= 0 {
		logicalWidth := w.calculateTextWidth(text)
		minY, maxY := w.calculateYBounds(text)
		logicalHeight := math.Max(0.01, maxY-minY)

		oneLineScale := math.Min(
			usableWidth/math.Max(logicalWidth, 0.01),
			(mapHeight*w.singleLineHeightRatio)/logicalHeight,
		)

		if oneLineScale >= float64(w.defaultWrapFontPx) {
			scale := math.Min(oneLineScale, float64(w.maxFontPx))
			return []string{text}, scale, minY, maxY
		}
	}

	startPx := w.config.FontSizePx
	if startPx <= 0 {
		startPx = w.defaultWrapFontPx
	}
	startPx = minInt(w.maxFontPx, maxInt(w.minFontPx, startPx))

	minReadablePx := w.minFontPx
	maxLines := w.maxManualLines
	if w.config.FontSizePx <= 0 {
		minReadablePx = w.autoMinReadablePx
		maxLines = w.maxAutoLines
	}

	maxAreaHeight := mapHeight * w.multiLineHeightRatio

	bestLines := []string{}
	bestScale := 0.0
	bestMinY, bestMaxY := 0.0, 1.0
	bestPenalty := int(^uint(0) >> 1)

	for px := startPx; px >= w.absoluteMinFontPx; px-- {
		scale := float64(px)
		lines := w.wrapText(text, scale, usableWidth)
		if len(lines) == 0 {
			continue
		}

		minY, maxY := w.maxLineBounds(lines)
		glyphHeight := math.Max(1.0, maxY-minY) * scale
		totalHeight := glyphHeight
		if len(lines) > 1 {
			totalHeight += float64(len(lines)-1) * (w.linePitchRatio * scale)
		}

		heightOverflow := totalHeight - maxAreaHeight
		lineOverflow := len(lines) - maxLines
		if heightOverflow < 0 {
			heightOverflow = 0
		}
		if lineOverflow < 0 {
			lineOverflow = 0
		}

		penalty := int(math.Ceil(heightOverflow))*100 + lineOverflow*500 + len(lines)*10 - px
		if penalty < bestPenalty {
			bestPenalty = penalty
			bestLines = lines
			bestScale = scale
			bestMinY = minY
			bestMaxY = maxY
		}

		// Сначала стараемся не опускаться ниже читаемого порога.
		if totalHeight <= maxAreaHeight && len(lines) <= maxLines && px >= minReadablePx {
			return lines, scale, minY, maxY
		}
		// Но если сообщение длинное, разрешаем уменьшиться ещё немного,
		// лишь бы оно корректно уложилось вниз по строкам.
		if totalHeight <= maxAreaHeight && len(lines) <= maxLines {
			return lines, scale, minY, maxY
		}
	}

	if len(bestLines) > 0 {
		return bestLines, bestScale, bestMinY, bestMaxY
	}

	scale := float64(w.absoluteMinFontPx)
	lines := w.wrapText(text, scale, usableWidth)
	minY, maxY := w.maxLineBounds(lines)
	return lines, scale, minY, maxY
}

func (w *Writer) waitPhysicalPenUp(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	stableSince := time.Time{}

	for time.Now().Before(deadline) {
		leftDown := asyncKeyDown(0x01) // VK_LBUTTON
		ctrlDown := asyncKeyDown(vkControl)

		if !leftDown && !ctrlDown {
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= 12*time.Millisecond {
				return
			}
		} else {
			stableSince = time.Time{}
			w.mouseUp()
			w.keyUp(vkControl)
		}

		time.Sleep(2 * time.Millisecond)
	}
}

func (w *Writer) firstDrawablePoint(line string, startX, originY, scale float64) (float64, float64, bool) {
	cursorX := startX
	spacing := w.effectiveLetterSpacing(scale)
	previousLetter := false

	for _, r := range line {
		ch := string(r)

		if ch == " " {
			cursorX += w.spaceWidth * scale
			previousLetter = false
			continue
		}

		glyph, ok := w.font[ch]
		if !ok {
			continue
		}

		if previousLetter {
			cursorX += spacing * scale
		}

		for _, stroke := range glyph.Strokes {
			if len(stroke) == 0 || len(stroke[0]) < 2 {
				continue
			}

			p := stroke[0]
			x := cursorX + (p[0]-glyph.MinX)*scale
			y := originY + p[1]*scale
			return x, y, true
		}

		cursorX += glyph.Width * scale
		previousLetter = true
	}

	return 0, 0, false
}

func (w *Writer) prepareNextLine(line string, startX, originY, scale float64) error {
	if err := w.requireDotaForeground(); err != nil {
		return err
	}

	// Stop drawing completely and let Dota consume the release events.
	w.releaseDrawMode()
	time.Sleep(w.lineBreakSettleDelay)

	w.mouseUp()
	w.keyUp(vkControl)
	w.waitPhysicalPenUp(140 * time.Millisecond)

	if err := w.requireDotaForeground(); err != nil {
		return err
	}

	// Teleport directly to the first stroke of the next line.
	// No diagonal mouse path is generated.
	if x, y, ok := w.firstDrawablePoint(line, startX, originY, scale); ok {
		w.hardWarpPixel(x, y)
		time.Sleep(25 * time.Millisecond)
	}

	// Confirm release once more after the warp.
	w.mouseUp()
	w.keyUp(vkControl)
	w.waitPhysicalPenUp(70 * time.Millisecond)

	return nil
}

func (w *Writer) separateLines() error {
	if err := w.requireDotaForeground(); err != nil {
		return err
	}
	w.releaseDrawMode()
	w.waitPhysicalPenUp(100 * time.Millisecond)
	return nil
}

func (w *Writer) drawLine(line string, startX, originY, scale float64) error {
	cursorX := startX
	previousLetter := false
	previousRight := 0.0
	spacing := w.effectiveLetterSpacing(scale)

	for _, r := range line {
		if err := w.requireDotaForeground(); err != nil {
			return err
		}

		ch := string(r)
		if ch == " " {
			w.releaseDrawMode()
			cursorX += w.spaceWidth * scale
			previousLetter = false
			previousRight = 0
			continue
		}

		glyph, ok := w.font[ch]
		if !ok {
			continue
		}

		if previousLetter {
			if err := w.separateLetters(previousRight, originY, cursorX, scale); err != nil {
				return err
			}
		}

		if err := w.drawCharacter(ch, cursorX, originY, scale); err != nil {
			return err
		}

		previousRight = cursorX + glyph.Width*scale
		cursorX += glyph.Width*scale + spacing*scale
		previousLetter = true
	}

	// Строка закончилась — не оставляем состояние рисования
	// на волю очереди событий Dota.
	w.releaseDrawMode()
	return nil
}


// v1.8: automatic minimap side detection.
// Keeps manual calibration if available; otherwise uses screen side.
func (w *Writer) autoDetectMapSide() {
	if w.config.MapCoords != nil {
		return
	}

	// Dota default HUD: minimap is normally bottom-left.
	// If the left area does not fit, fallback to right side.
	size := int(float64(w.screenHeight) * w.mapSizeRatio)
	if size < 180 {
		size = 180
	}

	if w.screenWidth > 1600 {
		w.mapCoords = MapCoords{
			TopLeft: [2]int{8, w.screenHeight-size-8},
			BottomRight: [2]int{8+size, w.screenHeight-8},
			Screen: [2]int{w.screenWidth,w.screenHeight},
		}
		return
	}

	w.mapCoords = MapCoords{
		TopLeft: [2]int{8, w.screenHeight-size-8},
		BottomRight: [2]int{8+size, w.screenHeight-8},
		Screen: [2]int{w.screenWidth,w.screenHeight},
	}
}

func (w *Writer) drawText(text string) error {
	text = strings.ToUpper(strings.TrimSpace(text))
	if text == "" {
		return nil
	}

	if err := w.requireDotaForeground(); err != nil {
		return err
	}

	w.autoDetectMapSide()

	x1, y1 := w.mapCoords.TopLeft[0], w.mapCoords.TopLeft[1]
	x2, y2 := w.mapCoords.BottomRight[0], w.mapCoords.BottomRight[1]
	mapWidth := float64(x2 - x1)
	mapHeight := float64(y2 - y1)

	lines, scale, minY, maxY := w.layoutText(text, mapWidth, mapHeight)
	if len(lines) == 0 {
		return nil
	}

	glyphHeight := math.Max(1.0, maxY-minY) * scale
	totalHeight := glyphHeight
	if len(lines) > 1 {
		totalHeight += float64(len(lines)-1) * (w.linePitchRatio * scale)
	}

	top := float64(y1) + (mapHeight-totalHeight)/2

	fmt.Printf("\nРисую: %s\n", text)
	qualityMode := "NORMAL"
	if w.smallFontMode(scale) {
		qualityMode = "COMPACT"
	}
	fmt.Printf("Шрифт: %.0f px | строк: %d | режим: %s | качество: %s\n",
		scale, len(lines), w.fontModeString(), qualityMode)

	w.releaseDrawMode()
	defer w.releaseDrawMode()

	for i, line := range lines {
		logicalWidth := w.calculateTextWidthAtScale(line, scale)
		actualWidth := logicalWidth * scale
		startX := float64(x1) + (mapWidth-actualWidth)/2

		lineTop := top + float64(i)*(w.linePitchRatio*scale)
		originY := lineTop - minY*scale

		if i > 0 {
			if err := w.prepareNextLine(line, startX, originY, scale); err != nil {
				return err
			}
		}

		if err := w.drawLine(line, startX, originY, scale); err != nil {
			return err
		}
	}

	return nil
}

func (w *Writer) getKeyboardLayout() uintptr {
	hwnd, _, _ := procGetForegroundWindow.Call()
	var pid uint32
	threadID, _, _ := procGetWindowThreadProcessId.Call(
		hwnd,
		uintptr(unsafe.Pointer(&pid)),
	)
	layout, _, _ := procGetKeyboardLayout.Call(threadID)
	return layout
}

func (w *Writer) vkToChar(vk int) string {
	var state [256]byte
	ok, _, _ := procGetKeyboardState.Call(uintptr(unsafe.Pointer(&state[0])))
	if ok == 0 {
		return ""
	}

	layout := w.getKeyboardLayout()
	scan, _, _ := procMapVirtualKeyExW.Call(uintptr(vk), 0, layout)

	var buffer [8]uint16
	result, _, _ := procToUnicodeEx.Call(
		uintptr(vk), scan,
		uintptr(unsafe.Pointer(&state[0])),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)), 0, layout,
	)

	n := int32(result)
	if n <= 0 {
		return ""
	}
	return string(utf16.Decode(buffer[:n]))
}

func asyncKeyState(vk int) (down bool, pressedSinceLastCall bool) {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	state := uint16(r & 0xffff)

	down = (state & 0x8000) != 0
	pressedSinceLastCall = (state & 0x0001) != 0
	return
}

func asyncKeyDown(vk int) bool {
	down, _ := asyncKeyState(vk)
	return down
}

func keyPressed(vk int, previousDown bool) (down bool, pressed bool) {
	down, sinceLast := asyncKeyState(vk)

	// Either a normal up->down edge OR a complete fast tap
	// that happened between two iterations.
	pressed = sinceLast || (down && !previousDown)
	return
}

func (w *Writer) getCursorPosition() ([2]int, bool) {
	var p PointI
	ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return [2]int{int(p.X), int(p.Y)}, ok != 0
}

func (w *Writer) saveMapTopLeft() {
	p, ok := w.getCursorPosition()
	if !ok {
		fmt.Println("\nНе удалось получить координаты курсора.")
		return
	}
	w.pendingTopLeft = &p
	fmt.Printf("\nF6: левый верхний угол миникарты = %v\n", p)
	fmt.Println("Теперь наведите курсор на ПРАВЫЙ НИЖНИЙ угол и нажмите F7.")
}

func (w *Writer) saveMapBottomRight() {
	p, ok := w.getCursorPosition()
	if !ok {
		fmt.Println("\nНе удалось получить координаты курсора.")
		return
	}
	if w.pendingTopLeft == nil {
		fmt.Println("\nСначала наведите курсор на левый верхний угол и нажмите F6.")
		return
	}

	x1, y1 := (*w.pendingTopLeft)[0], (*w.pendingTopLeft)[1]
	x2, y2 := p[0], p[1]
	if x2 <= x1 || y2 <= y1 {
		fmt.Println("\nНекорректная область миникарты.")
		return
	}
	if x2-x1 < 100 || y2-y1 < 100 {
		fmt.Println("\nОбласть слишком маленькая. Повторите F6/F7.")
		return
	}

	c := &MapCoords{
		TopLeft:     [2]int{x1, y1},
		BottomRight: [2]int{x2, y2},
		Screen:      [2]int{w.screenWidth, w.screenHeight},
	}
	w.mapCoords = *c
	w.config.MapCoords = c
	w.saveConfig()
	w.pendingTopLeft = nil
	fmt.Printf("\nКалибровка сохранена: %v -> %v\n", c.TopLeft, c.BottomRight)
}

func (w *Writer) resetMapCalibration() {
	w.config.MapCoords = nil
	w.pendingTopLeft = nil
	w.saveConfig()
	w.setupMap()
	fmt.Printf("\nКалибровка сброшена. Авто-область: %v -> %v\n",
		w.mapCoords.TopLeft, w.mapCoords.BottomRight)
}

func (w *Writer) showBuffer() {
	fmt.Printf("\r[ЧАТ] %s                                                  ", w.inputBuffer)
}

func uniqueKeys(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, k := range in {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (w *Writer) drawAsync(text string) {
	time.Sleep(w.startDrawDelay)

	if err := w.requireDotaForeground(); err != nil {
		fmt.Println("\nРисование остановлено: Dota 2 не в фокусе.")
		w.drawing.Store(false)
		return
	}

	started := time.Now()
	err := w.drawText(text)
	if errors.Is(err, errDrawInterrupted) {
		fmt.Println("\nРисование остановлено: Dota 2 не в фокусе.")
	} else if err != nil {
		fmt.Printf("\nОшибка: %v\n", err)
	} else {
		fmt.Printf("\nГотово за %.2f сек.\n", time.Since(started).Seconds())
	}

	w.releaseDrawMode()
	w.drawing.Store(false)
	fmt.Print("\nБуфер: ")
}

func (w *Writer) monitor() {
	normal := make([]int, 0, 80)
	for i := 0x41; i <= 0x5A; i++ {
		normal = append(normal, i)
	}
	for i := 0x30; i <= 0x39; i++ {
		normal = append(normal, i)
	}
	normal = append(normal,
		0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF,
		0xC0, 0xDB, 0xDC, 0xDD, 0xDE, 0xE2,
	)

	tracked := append([]int{}, normal...)
	tracked = append(tracked,
		vkEnter, vkBackspace, vkSpace, vkEscape,
		vkF2, vkF3, vkF4, vkF6, vkF7, vkF8,
	)
	tracked = uniqueKeys(tracked)

	previous := map[int]bool{}
	syncStates := func() {
		for _, k := range tracked {
			previous[k] = asyncKeyDown(k)
		}
	}

	fmt.Print("Буфер: ")

	for w.running.Load() {
		if asyncKeyDown(vkF9) {
			w.running.Store(false)
			fmt.Println("\nВыход.")
			break
		}

		dotaActive := w.isDotaForeground()
		if !dotaActive {
			if w.wasDotaForeground {
				w.inputBuffer = ""
				w.chatCapture = false
				w.releaseDrawMode()
				fmt.Print("\r[ПАУЗА] Dota 2 не в фокусе                              ")
			}
			w.wasDotaForeground = false
			syncStates()
			time.Sleep(25 * time.Millisecond)
			continue
		}

		if !w.wasDotaForeground {
			syncStates()
			fmt.Printf("\rБуфер: %s                                                  ", w.inputBuffer)
		}
		w.wasDotaForeground = true

		f2, f2Pressed := keyPressed(vkF2, previous[vkF2])
		if f2Pressed {
			w.fontSmaller()
		}
		previous[vkF2] = f2

		f3, f3Pressed := keyPressed(vkF3, previous[vkF3])
		if f3Pressed {
			w.fontLarger()
		}
		previous[vkF3] = f3

		f4, f4Pressed := keyPressed(vkF4, previous[vkF4])
		if f4Pressed {
			w.fontAuto()
		}
		previous[vkF4] = f4

		f6, f6Pressed := keyPressed(vkF6, previous[vkF6])
		if f6Pressed {
			w.saveMapTopLeft()
		}
		previous[vkF6] = f6

		f7, f7Pressed := keyPressed(vkF7, previous[vkF7])
		if f7Pressed {
			w.saveMapBottomRight()
		}
		previous[vkF7] = f7

		f8, f8Pressed := keyPressed(vkF8, previous[vkF8])
		if f8Pressed {
			w.resetMapCalibration()
		}
		previous[vkF8] = f8

		esc, escPressed := keyPressed(vkEscape, previous[vkEscape])
		if escPressed {
			w.inputBuffer = ""
			w.chatCapture = false
			fmt.Print("\rБуфер очищен.                                             ")
		}
		previous[vkEscape] = esc

		if w.drawing.Load() {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		enter, enterPressed := keyPressed(vkEnter, previous[vkEnter])
		if enterPressed {
			if !w.chatCapture {
				w.chatCapture = true
				w.inputBuffer = ""
				fmt.Print("\r[ЧАТ]                                                       ")
			} else {
				text := strings.TrimSpace(w.inputBuffer)
				w.chatCapture = false
				w.inputBuffer = ""
				if text != "" {
					w.drawing.Store(true)
					go w.drawAsync(text)
				}
			}
		}
		previous[vkEnter] = enter

		back, backPressed := keyPressed(vkBackspace, previous[vkBackspace])
		if w.chatCapture && backPressed {
			r := []rune(w.inputBuffer)
			if len(r) > 0 {
				w.inputBuffer = string(r[:len(r)-1])
			}
			w.showBuffer()
		}
		previous[vkBackspace] = back

		space, spacePressed := keyPressed(vkSpace, previous[vkSpace])
		if w.chatCapture && spacePressed {
			w.inputBuffer += " "
			w.showBuffer()
		}
		previous[vkSpace] = space

		for _, vk := range normal {
			down, pressed := keyPressed(vk, previous[vk])
			if w.chatCapture && pressed {
				ch := w.vkToChar(vk)
				if ch != "" {
					w.inputBuffer += ch
					w.showBuffer()
				}
			}
			previous[vk] = down
		}

		time.Sleep(1 * time.Millisecond)
	}
}

func main() {
	fmt.Println("============================================================")
	fmt.Println("DOTA 2 MAP WRITER — STANDALONE v1.1")
	fmt.Println("============================================================")

	w, err := newWriter()
	if err != nil {
		fmt.Printf("Ошибка запуска: %v\n", err)
		fmt.Println("Нажмите Enter для выхода.")
		fmt.Scanln()
		return
	}
	defer w.close()

	fmt.Printf("Экран: %dx%d\n", w.screenWidth, w.screenHeight)
	fmt.Printf("Миникарта: %v -> %v\n", w.mapCoords.TopLeft, w.mapCoords.BottomRight)
	fmt.Printf("Шрифт: %s\n", w.fontModeString())
	fmt.Println()
	fmt.Println("F2 — уменьшить шрифт на 2 px (минимум 24)")
	fmt.Println("F3 — увеличить шрифт на 2 px")
	fmt.Println("F4 — автоматический размер")
	fmt.Println("F6 — левый верхний угол миникарты")
	fmt.Println("F7 — правый нижний угол миникарты и сохранить")
	fmt.Println("F8 — сбросить калибровку")
	fmt.Println("Длинный текст: перенос вниз, при нехватке места — автоматическое уменьшение")
	fmt.Println("F9 — выход")
	fmt.Println("Alt+Tab — автоматическая пауза")
	fmt.Println("Ввод v1.6: быстрые нажатия больше не должны теряться")
	fmt.Println("Спецсимволы: . , : ; ( ) [ ] { } / \\ _ = < > # @ % & * и др.")
	fmt.Println()
	fmt.Println("Длинные сообщения автоматически переносятся на несколько строк,")
	fmt.Println("чтобы буквы не уменьшались до нечитаемого размера.")
	fmt.Println()
	fmt.Println("Python НЕ требуется.")
	fmt.Println()

	w.monitor()
}
