package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ReplayRecordModeMatch   = "match"
	ReplayRecordModeSession = "session"

	// Replay files store the same payload for both delay-based and rollback netplay:
	// 2 bytes of digital inputs followed by 6 bytes of signed analog axes per controller.
	REPLAY_NUM_INPUTS  = MaxSimul * 2
	REPLAY_INPUT_BYTES = 2 + 6
)

const (
	replayHeaderMagic   = "IKR2"
	replayHeaderVersion = uint16(1)
)

const (
	ReplayFileKindLegacy uint16 = iota
	ReplayFileKindSession
	ReplayFileKindMatch
)

func writeReplayInput(w io.Writer, ibit InputBits, axes [6]int8) error {
	var buf [REPLAY_INPUT_BYTES]byte
	binary.LittleEndian.PutUint16(buf[:2], uint16(ibit))
	for i := 0; i < len(axes); i++ {
		buf[2+i] = byte(axes[i])
	}
	_, err := w.Write(buf[:])
	return err
}

func readReplayInput(r io.Reader, ibit *InputBits, axes *[6]int8) error {
	var buf [REPLAY_INPUT_BYTES]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err
	}
	*ibit = InputBits(int16(binary.LittleEndian.Uint16(buf[:2])))
	for i := 0; i < len(axes); i++ {
		axes[i] = int8(buf[2+i])
	}
	return nil
}

type ReplaySelectedChar struct {
	Def string `json:"def"`
	Pal int32  `json:"pal"`
}

type ReplayMatchConfig struct {
	GameMode   string                  `json:"gameMode"`
	HomeTeam   int32                   `json:"homeTeam"`
	MatchWins  [2]int32                `json:"matchWins"`
	RoundTime  int32                   `json:"roundTime"`
	TeamModes  [2]int32                `json:"teamModes"`
	NumSimul   [2]int32                `json:"numSimul"`
	NumTurns   [2]int32                `json:"numTurns"`
	AILevels   [MaxPlayerNo]float32    `json:"aiLevels"`
	StageDef   string                  `json:"stageDef"`
	Chars      [2][]ReplaySelectedChar `json:"chars"`
	GameParams []string                `json:"gameParams"`
}

type ReplayFile struct {
	file         *os.File
	ibit         [REPLAY_NUM_INPUTS]InputBits
	iaxes        [REPLAY_NUM_INPUTS][6]int8
	seed         int32
	preMatchTime int32
	inputOffset  int64
	kind         uint16
	matchConfig  *ReplayMatchConfig
}

func normalizeReplayDefPath(path string) string {
	return strings.ToLower(filepath.ToSlash(strings.TrimSpace(path)))
}

func OpenReplayFile(filename string) *ReplayFile {
	rf, err := os.Open(filename)
	if err != nil {
		log.Printf("Failed to open replay file %s: %v", filename, err)
		return nil
	}
	replay := &ReplayFile{file: rf, inputOffset: 8, kind: ReplayFileKindLegacy}
	if err := replay.readHeader(); err != nil {
		log.Printf("Failed to parse replay file %s: %v", filename, err)
		replay.Close()
		return nil
	}
	log.Printf("Replay file opened: %s", filename)
	return replay
}

func (rf *ReplayFile) readHeader() error {
	if rf.file == nil {
		return fmt.Errorf("replay file is not open")
	}
	if _, err := rf.file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	var magic [4]byte
	if _, err := io.ReadFull(rf.file, magic[:]); err != nil {
		return err
	}
	if string(magic[:]) != replayHeaderMagic {
		if _, err := rf.file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := binary.Read(rf.file, binary.LittleEndian, &rf.seed); err != nil {
			return err
		}
		if err := binary.Read(rf.file, binary.LittleEndian, &rf.preMatchTime); err != nil {
			return err
		}
		pos, err := rf.file.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		rf.inputOffset = pos
		rf.kind = ReplayFileKindLegacy
		return nil
	}

	var version uint16
	if err := binary.Read(rf.file, binary.LittleEndian, &version); err != nil {
		return err
	}
	if version != replayHeaderVersion {
		return fmt.Errorf("unsupported replay header version: %d", version)
	}
	if err := binary.Read(rf.file, binary.LittleEndian, &rf.kind); err != nil {
		return err
	}
	if err := binary.Read(rf.file, binary.LittleEndian, &rf.seed); err != nil {
		return err
	}
	if err := binary.Read(rf.file, binary.LittleEndian, &rf.preMatchTime); err != nil {
		return err
	}
	var metaLen uint32
	if err := binary.Read(rf.file, binary.LittleEndian, &metaLen); err != nil {
		return err
	}
	if metaLen > 0 {
		meta := make([]byte, metaLen)
		if _, err := io.ReadFull(rf.file, meta); err != nil {
			return err
		}
		if rf.kind == ReplayFileKindMatch {
			var cfg ReplayMatchConfig
			if err := json.Unmarshal(meta, &cfg); err != nil {
				return err
			}
			rf.matchConfig = &cfg
		}
	}
	pos, err := rf.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	rf.inputOffset = pos
	return nil
}

func (rf *ReplayFile) HasMatchConfig() bool {
	return rf != nil && rf.matchConfig != nil
}

func (rf *ReplayFile) Close() {
	if rf.file != nil {
		rf.file.Close()
		rf.file = nil
	}
}

func (rf *ReplayFile) readReplayInput(i int) [14]bool {
	if i >= 0 && i < len(rf.ibit) {
		return rf.ibit[i].BitsToKeys()
	}
	return [14]bool{}
}

func (rf *ReplayFile) readReplayInputAnalog(i int) [6]int8 {
	if i >= 0 && i < len(rf.iaxes) {
		return rf.iaxes[i]
	}
	return [6]int8{}
}

func (rf *ReplayFile) AnyButton() bool {
	for _, b := range rf.ibit {
		if b&IB_anybutton != 0 {
			return true
		}
	}
	return false
}

func (rf *ReplayFile) Synchronize() {
	if rf.file == nil {
		return
	}
	if _, err := rf.file.Seek(rf.inputOffset, io.SeekStart); err != nil {
		log.Printf("Failed to seek replay file: %v", err)
		sys.esc = true
		return
	}
	Srand(rf.seed)
	rf.Update()
	log.Printf("Replay synchronized: seed=%d pmTime=%d kind=%d", rf.seed, rf.preMatchTime, rf.kind)
}

func (rf *ReplayFile) Update() bool {
	if rf.file == nil {
		sys.esc = true
	} else {
		if sys.oldNextAddTime > 0 {
			rf.ibit = [REPLAY_NUM_INPUTS]InputBits{}
			rf.iaxes = [REPLAY_NUM_INPUTS][6]int8{}

			for i := 0; i < len(rf.ibit); i++ {
				if err := readReplayInput(rf.file, &rf.ibit[i], &rf.iaxes[i]); err != nil {
					if err == io.EOF || err == io.ErrUnexpectedEOF {
						log.Printf("Closing replay file")
					} else {
						log.Printf("Error while reading replay input for controller %d: %v", i, err)
					}
					sys.esc = true
					break
				}
			}
		}

		if sys.esc {
			log.Printf("Closing replay file")
			rf.Close()
		}
	}
	return !sys.gameEnd
}

func findReplayCharRef(def string) int {
	norm := normalizeReplayDefPath(def)
	for i := range sys.sel.charlist {
		if normalizeReplayDefPath(sys.sel.charlist[i].def) == norm {
			return i
		}
	}
	return -1
}

func findReplayStageNo(def string) int {
	norm := normalizeReplayDefPath(def)
	for i := range sys.sel.stagelist {
		if normalizeReplayDefPath(sys.sel.stagelist[i].def) == norm {
			return i + 1
		}
	}
	return -1
}

func (rf *ReplayFile) applyMatchConfig() error {
	if rf.matchConfig == nil {
		return fmt.Errorf("replay does not contain match configuration")
	}
	cfg := rf.matchConfig
	sys.sel.ClearSelected()
	if cfg.GameMode != "" {
		sys.gameMode = cfg.GameMode
		sys.replayManager.onModeChange(sys.gameMode)
	}
	if cfg.HomeTeam >= 0 && cfg.HomeTeam <= 1 {
		sys.home = int(cfg.HomeTeam)
	}
	for side := 0; side < 2; side++ {
		tm := TeamMode(cfg.TeamModes[side])
		if tm < 0 || tm > TM_LAST {
			tm = TM_Single
		}
		sys.tmode[side] = tm
		sys.numSimul[side] = Max(1, cfg.NumSimul[side])
		sys.numTurns[side] = Max(1, cfg.NumTurns[side])
		for _, ch := range cfg.Chars[side] {
			ref := findReplayCharRef(ch.Def)
			if ref < 0 {
				sc := sys.sel.AddChar(ch.Def)
				if sc == nil || len(sys.sel.charlist) == 0 {
					return fmt.Errorf("failed to add replay character: %s", ch.Def)
				}
				ref = len(sys.sel.charlist) - 1
				if normalizeReplayDefPath(sys.sel.charlist[ref].def) != normalizeReplayDefPath(ch.Def) {
					return fmt.Errorf("failed to resolve replay character: %s", ch.Def)
				}
			}
			pal := sys.sel.ValidatePalette(ref, int(ch.Pal))
			if !sys.sel.AddSelectedChar(side, ref, pal) {
				return fmt.Errorf("failed to select replay character: %s", ch.Def)
			}
		}
	}
	sys.aiLevel = cfg.AILevels
	if cfg.StageDef != "" {
		stageNo := findReplayStageNo(cfg.StageDef)
		if stageNo < 0 {
			if _, err := sys.sel.AddStage(cfg.StageDef); err != nil {
				return fmt.Errorf("failed to add replay stage: %w", err)
			}
			stageNo = len(sys.sel.stagelist)
		}
		sys.sel.SelectStage(stageNo)
	}
	if sys.sel.gameParams == nil {
		sys.sel.gameParams = newGameParams()
	} else {
		sys.sel.gameParams.Reset()
	}
	sys.sel.music = make(Music)
	if len(cfg.GameParams) > 0 {
		sys.sel.gameParams.AppendParams(cfg.GameParams)
		sys.sel.music.AppendParams(sys.sel.gameParams.MusicEntries())
	}
	sys.maxRoundTime = cfg.RoundTime
	sys.lifebar.ro.match_wins = cfg.MatchWins
	return nil
}
func (rf *ReplayFile) PlayConfiguredMatch() error {
	if err := rf.applyMatchConfig(); err != nil {
		return err
	}
	sys.loadStart()
	return nil
}

type ReplayCapture struct {
	file        *os.File
	path        string
	kind        uint16
	matchConfig *ReplayMatchConfig
}

type ReplayManager struct {
	appSessionTimestamp string
	netplayTimestamp    string
	modeKey             string
	modeTimestamp       string
	matchCounters       map[string]int
	sessionCapture      *ReplayCapture
	activeCapture       *ReplayCapture
}

func (rm *ReplayManager) normalizeMode(mode string) string {
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "" {
		return ""
	}
	return sanitizeReplayName(mode)
}

func (rm *ReplayManager) ensureAppSession() {
	if rm.appSessionTimestamp == "" {
		rm.appSessionTimestamp = replayTimestamp()
	}
	if rm.matchCounters == nil {
		rm.matchCounters = make(map[string]int)
	}
}

func sanitizeReplayName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return "unknown"
	}
	replacer := strings.NewReplacer(
		"\\", "_",
		"/", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	)
	name = replacer.Replace(name)
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}
	return strings.Trim(name, "._")
}

func normalizeReplayDir(path string, fallback string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		path = fallback
	}
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimRight(path, "/")
	if path == "" {
		return fallback
	}
	return path
}

func (rm *ReplayManager) recordingMode() string {
	mode := strings.TrimSpace(strings.ToLower(sys.cfg.Replay.Recording))
	if mode != ReplayRecordModeSession {
		mode = ReplayRecordModeMatch
	}
	return mode
}

func (rm *ReplayManager) replayRootDir() string {
	root := normalizeReplayDir(sys.cfg.Replay.Folder, "save/replays")
	rm.ensureAppSession()
	return filepath.Join(root, "session_"+rm.appSessionTimestamp)
}

func (rm *ReplayManager) disabledModes() map[string]bool {
	out := make(map[string]bool, len(sys.cfg.Replay.DisabledModes))
	for _, mode := range sys.cfg.Replay.DisabledModes {
		mode = rm.normalizeMode(mode)
		if mode != "" {
			out[mode] = true
		}
	}
	return out
}

func (rm *ReplayManager) shouldRecordMode(mode string) bool {
	mode = rm.normalizeMode(mode)
	if mode == "" || sys.replayFile != nil {
		return false
	}
	if disabled := rm.disabledModes(); disabled[mode] {
		return false
	}
	return true
}

func (rm *ReplayManager) beginNetplaySession() {
	rm.ensureAppSession()
	if rm.netplayTimestamp == "" {
		rm.netplayTimestamp = replayTimestamp()
	}
}

func (rm *ReplayManager) endNetplaySession() {
	rm.closeModeSession()
	rm.netplayTimestamp = ""
}

func (rm *ReplayManager) onModeChange(mode string) {
	mode = rm.normalizeMode(mode)
	if mode == rm.modeKey {
		if mode != "" && rm.modeTimestamp == "" {
			rm.modeTimestamp = replayTimestamp()
		}
		return
	}
	rm.closeModeSession()
	rm.modeKey = mode
	if mode == "" {
		rm.modeTimestamp = ""
		return
	}
	rm.modeTimestamp = replayTimestamp()
}

func (rm *ReplayManager) modeParentDir(mode string) string {
	root := rm.replayRootDir()
	if strings.HasPrefix(mode, "netplay") && rm.netplayTimestamp != "" {
		return filepath.Join(root, "netplay_"+rm.netplayTimestamp)
	}
	return root
}

func (rm *ReplayManager) modeDir(mode string) string {
	if mode == "" {
		return ""
	}
	rm.ensureAppSession()
	if rm.modeKey != mode || rm.modeTimestamp == "" {
		rm.modeKey = mode
		rm.modeTimestamp = replayTimestamp()
	}
	return filepath.Join(rm.modeParentDir(mode), mode+"_"+rm.modeTimestamp)
}

func (rm *ReplayManager) nextMatchPath(modeDir string) string {
	rm.ensureAppSession()
	rm.matchCounters[modeDir]++
	return filepath.Join(modeDir, fmt.Sprintf("match_%02d.replay", rm.matchCounters[modeDir]))
}

func (rm *ReplayManager) sessionPath(modeDir string) string {
	return filepath.Join(modeDir, "session.replay")
}

func (rm *ReplayManager) buildMatchConfig() *ReplayMatchConfig {
	cfg := &ReplayMatchConfig{
		GameMode:   sys.gameMode,
		HomeTeam:   int32(sys.home),
		MatchWins:  sys.lifebar.ro.match_wins,
		RoundTime:  sys.maxRoundTime,
		AILevels:   sys.aiLevel,
		StageDef:   "",
		GameParams: nil,
	}
	for side := 0; side < 2; side++ {
		cfg.TeamModes[side] = int32(sys.tmode[side])
		cfg.NumSimul[side] = sys.numSimul[side]
		cfg.NumTurns[side] = sys.numTurns[side]
		for _, selected := range sys.sel.selected[side] {
			if selected[0] < 0 || selected[0] >= len(sys.sel.charlist) {
				continue
			}
			sc := sys.sel.charlist[selected[0]]
			cfg.Chars[side] = append(cfg.Chars[side], ReplaySelectedChar{
				Def: sc.def,
				Pal: int32(selected[1]),
			})
		}
	}
	if sys.stage != nil && sys.stage.def != "" {
		cfg.StageDef = sys.stage.def
	} else if sys.sel.selectedStageNo > 0 && sys.sel.selectedStageNo <= len(sys.sel.stagelist) {
		cfg.StageDef = sys.sel.stagelist[sys.sel.selectedStageNo-1].def
	}
	if sys.sel.gameParams != nil && len(sys.sel.gameParams.Raw) > 0 {
		cfg.GameParams = append([]string{}, sys.sel.gameParams.Raw...)
	}
	return cfg
}

func (rm *ReplayManager) captureForFile(file *os.File) *ReplayCapture {
	if file == nil {
		return nil
	}
	name := file.Name()
	captures := []*ReplayCapture{rm.activeCapture, rm.sessionCapture}
	for _, capture := range captures {
		if capture != nil && capture.file != nil && capture.file.Name() == name {
			return capture
		}
	}
	return nil
}

func openReplayCapture(path string, kind uint16, matchConfig *ReplayMatchConfig) (*ReplayCapture, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &ReplayCapture{file: f, path: path, kind: kind, matchConfig: matchConfig}, nil
}

func (rc *ReplayCapture) Close() {
	if rc != nil && rc.file != nil {
		rc.file.Close()
		rc.file = nil
	}
}

func ensureReplayHeader(file *os.File, seed, preMatchTime int32) error {
	if file == nil {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		_, err = file.Seek(0, io.SeekEnd)
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	capture := sys.replayManager.captureForFile(file)
	if capture == nil {
		if err := binary.Write(file, binary.LittleEndian, &seed); err != nil {
			return err
		}
		if err := binary.Write(file, binary.LittleEndian, &preMatchTime); err != nil {
			return err
		}
		return nil
	}
	var meta []byte
	if capture.kind == ReplayFileKindMatch && capture.matchConfig != nil {
		meta, err = json.Marshal(capture.matchConfig)
		if err != nil {
			return err
		}
	}
	if _, err := file.Write([]byte(replayHeaderMagic)); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, replayHeaderVersion); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, capture.kind); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, &seed); err != nil {
		return err
	}
	if err := binary.Write(file, binary.LittleEndian, &preMatchTime); err != nil {
		return err
	}
	metaLen := uint32(len(meta))
	if err := binary.Write(file, binary.LittleEndian, metaLen); err != nil {
		return err
	}
	if metaLen > 0 {
		if _, err := file.Write(meta); err != nil {
			return err
		}
	}
	return nil
}

func (rm *ReplayManager) BeginMatchCapture() *os.File {
	mode := rm.normalizeMode(sys.gameMode)
	if !rm.shouldRecordMode(mode) {
		return nil
	}
	modeDir := rm.modeDir(mode)
	if modeDir == "" {
		return nil
	}

	if rm.recordingMode() == ReplayRecordModeSession {
		if rm.sessionCapture == nil {
			capture, err := openReplayCapture(rm.sessionPath(modeDir), ReplayFileKindSession, nil)
			if err != nil {
				sys.errLog.Printf("failed to create replay capture: %v", err)
				return nil
			}
			rm.sessionCapture = capture
		}
		rm.activeCapture = rm.sessionCapture
	} else {
		capture, err := openReplayCapture(rm.nextMatchPath(modeDir), ReplayFileKindMatch, rm.buildMatchConfig())
		if err != nil {
			sys.errLog.Printf("failed to create replay capture: %v", err)
			return nil
		}
		rm.activeCapture = capture
	}

	if sys.netConnection == nil && sys.rollback.session == nil {
		if err := ensureReplayHeader(rm.activeCapture.file, sys.randseed, sys.preMatchTime); err != nil {
			sys.errLog.Printf("failed to write local replay header: %v", err)
			rm.EndMatchCapture()
			return nil
		}
	}
	return rm.activeCapture.file
}

func (rm *ReplayManager) EndMatchCapture() {
	if rm.activeCapture == nil {
		return
	}
	if rm.recordingMode() == ReplayRecordModeSession && rm.activeCapture == rm.sessionCapture {
		return
	}
	rm.activeCapture.Close()
	rm.activeCapture = nil
	if sys.netConnection != nil {
		sys.netConnection.recording = nil
	}
	if sys.rollback.session != nil {
		sys.rollback.session.recording = nil
	}
}

func (rm *ReplayManager) closeModeSession() {
	if rm.sessionCapture != nil {
		rm.sessionCapture.Close()
	}
	rm.sessionCapture = nil
	rm.activeCapture = nil
	rm.modeKey = ""
	rm.modeTimestamp = ""
	if sys.netConnection != nil {
		sys.netConnection.recording = nil
	}
	if sys.rollback.session != nil {
		sys.rollback.session.recording = nil
	}
}

func (rm *ReplayManager) Shutdown() {
	rm.closeModeSession()
	rm.netplayTimestamp = ""
}

func (rm *ReplayManager) RecordLocalFrame(frames [REPLAY_NUM_INPUTS]ControllerFrameInput) {
	if rm.activeCapture == nil || rm.activeCapture.file == nil {
		return
	}
	for i := 0; i < REPLAY_NUM_INPUTS; i++ {
		if err := writeReplayInput(rm.activeCapture.file, frames[i].Bits, frames[i].Axes); err != nil {
			sys.errLog.Printf("failed to write local replay frame for controller %d: %v", i, err)
			rm.EndMatchCapture()
			return
		}
	}
}

func resolveReplayControllerFrames(read func(int) (ControllerFrameInput, bool)) [REPLAY_NUM_INPUTS]ControllerFrameInput {
	var frames [REPLAY_NUM_INPUTS]ControllerFrameInput
	for logical := 0; logical < REPLAY_NUM_INPUTS; logical++ {
		source := logical
		if logical < len(sys.inputRemap) {
			source = sys.inputRemap[logical]
		}
		if source < 0 {
			continue
		}
		if frame, ok := read(source); ok {
			frames[logical] = frame
		}
	}
	return frames
}

func (rm *ReplayManager) RecordRemappedFrame(read func(int) (ControllerFrameInput, bool)) {
	if rm.activeCapture == nil || rm.activeCapture.file == nil {
		return
	}
	rm.RecordLocalFrame(resolveReplayControllerFrames(read))
}
func replayTimestamp() string {
	return time.Now().Format("2006-01-02_03-04-05PM")
}

type ControllerFrameInput struct {
	Bits InputBits
	Axes [6]int8
}
type InputTape struct {
	Frames []ControllerFrameInput
}
type InputTapePlayback struct {
	Path   string
	Tape   *InputTape
	Frame  int
	Loop   bool
	Target int
}
type InputTapeRecording struct {
	Path       string
	Tape       InputTape
	Target     int
	Source     int
	MuteSource bool
}
type InputTapeManager struct {
	recording *InputTapeRecording
	playbacks map[int]*InputTapePlayback
}

func (tm *InputTapeManager) ensureMaps() {
	if tm.playbacks == nil {
		tm.playbacks = make(map[int]*InputTapePlayback)
	}
}
func normalizeInputTapePath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	return filepath.Clean(path)
}
func writeInputTape(path string, tape *InputTape) error {
	if tape == nil {
		return fmt.Errorf("input tape is nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte{'I', 'K', 'T', '1'}); err != nil {
		return err
	}
	frameCount := uint32(len(tape.Frames))
	if err := binary.Write(f, binary.LittleEndian, frameCount); err != nil {
		return err
	}
	for i := range tape.Frames {
		if err := writeReplayInput(f, tape.Frames[i].Bits, tape.Frames[i].Axes); err != nil {
			return err
		}
	}
	return nil
}
func readInputTape(path string) (*InputTape, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return nil, err
	}
	if string(magic[:]) != "IKT1" {
		return nil, fmt.Errorf("invalid input tape header")
	}
	var frameCount uint32
	if err := binary.Read(f, binary.LittleEndian, &frameCount); err != nil {
		return nil, err
	}
	tape := &InputTape{Frames: make([]ControllerFrameInput, frameCount)}
	for i := uint32(0); i < frameCount; i++ {
		if err := readReplayInput(f, &tape.Frames[i].Bits, &tape.Frames[i].Axes); err != nil {
			return nil, err
		}
	}
	return tape, nil
}
func validReplayInputIndex(idx int) bool {
	return idx >= 0 && idx < REPLAY_NUM_INPUTS
}
func (tm *InputTapeManager) StartRecording(path string, target, source int, muteSource bool) error {
	if !validReplayInputIndex(target) {
		return fmt.Errorf("invalid recording target %d", target+1)
	}
	if !validReplayInputIndex(source) {
		return fmt.Errorf("invalid recording source %d", source+1)
	}
	tm.ensureMaps()
	path = normalizeInputTapePath(path)
	delete(tm.playbacks, target)
	tm.recording = &InputTapeRecording{
		Path:       path,
		Target:     target,
		Source:     source,
		MuteSource: muteSource,
	}
	return nil
}
func (tm *InputTapeManager) StopRecording() (string, error) {
	if tm.recording == nil {
		return "", nil
	}
	path := tm.recording.Path
	tape := tm.recording.Tape
	tm.recording = nil
	return path, writeInputTape(path, &tape)
}
func (tm *InputTapeManager) StartPlayback(path string, target int, loop bool) error {
	if !validReplayInputIndex(target) {
		return fmt.Errorf("invalid playback target %d", target+1)
	}
	tm.ensureMaps()
	path = normalizeInputTapePath(path)
	tape, err := readInputTape(path)
	if err != nil {
		return err
	}
	if len(tape.Frames) == 0 {
		return fmt.Errorf("input tape is empty")
	}
	tm.playbacks[target] = &InputTapePlayback{
		Path:   path,
		Tape:   tape,
		Loop:   loop,
		Target: target,
	}
	return nil
}
func (tm *InputTapeManager) StopPlayback(target int) {
	if tm.playbacks == nil {
		return
	}
	delete(tm.playbacks, target)
}
func (tm *InputTapeManager) StopAll() {
	tm.recording = nil
	if tm.playbacks != nil {
		for target := range tm.playbacks {
			delete(tm.playbacks, target)
		}
	}
}
func (tm *InputTapeManager) RecordingActive() bool {
	return tm.recording != nil
}
func (tm *InputTapeManager) RecordingPath() string {
	if tm.recording == nil {
		return ""
	}
	return tm.recording.Path
}
func (tm *InputTapeManager) PlaybackActive(target int) bool {
	if tm.playbacks == nil {
		return false
	}
	_, ok := tm.playbacks[target]
	return ok
}
func (tm *InputTapeManager) ApplyFrame(base [REPLAY_NUM_INPUTS]ControllerFrameInput) [REPLAY_NUM_INPUTS]ControllerFrameInput {
	tm.ensureMaps()
	final := base
	if tm.recording != nil && validReplayInputIndex(tm.recording.Source) && validReplayInputIndex(tm.recording.Target) {
		final[tm.recording.Target] = base[tm.recording.Source]
		if tm.recording.MuteSource {
			final[tm.recording.Source] = ControllerFrameInput{}
		}
		tm.recording.Tape.Frames = append(tm.recording.Tape.Frames, final[tm.recording.Target])
	}
	for target, playback := range tm.playbacks {
		if playback == nil || playback.Tape == nil || len(playback.Tape.Frames) == 0 {
			delete(tm.playbacks, target)
			continue
		}
		if validReplayInputIndex(target) {
			final[target] = playback.Tape.Frames[playback.Frame]
		}
		playback.Frame++
		if playback.Frame >= len(playback.Tape.Frames) {
			if playback.Loop {
				playback.Frame = 0
			} else {
				delete(tm.playbacks, target)
			}
		}
	}
	return final
}
func inputTapeExists(path string) bool {
	_, err := os.Stat(normalizeInputTapePath(path))
	return err == nil
}
func deleteInputTape(path string) error {
	path = normalizeInputTapePath(path)
	if !inputTapeExists(path) {
		return nil
	}
	return os.Remove(path)
}
func (s *System) readBaseLocalControllerInput(controller int) ControllerFrameInput {
	if controller < 0 || controller >= len(s.inputRemap) {
		return ControllerFrameInput{}
	}
	in := s.inputRemap[controller]
	if in < 0 {
		return ControllerFrameInput{}
	}
	ir := NewInputReader()
	if controller < len(s.commandLists) && s.commandLists[controller] != nil && s.commandLists[controller].Buffer != nil {
		ir = s.commandLists[controller].Buffer.InputReader
	}
	buttons := ir.LocalInput(in)
	rawAxes := ir.LocalAnalogInput(in)
	var bits InputBits
	bits.KeysToBits(buttons)
	return ControllerFrameInput{Bits: bits, Axes: rawAxes}
}
func (s *System) prepareLocalControllerFrame() {
	if s.netConnection != nil || s.rollback.session != nil || s.replayFile != nil {
		s.localFramePrepared = false
		return
	}
	if s.localFramePrepared && s.localFramePreparedCount == s.frameCounter {
		return
	}
	var base [REPLAY_NUM_INPUTS]ControllerFrameInput
	for i := 0; i < REPLAY_NUM_INPUTS; i++ {
		base[i] = s.readBaseLocalControllerInput(i)
	}
	s.localFrameInputs = s.inputTapes.ApplyFrame(base)
	s.localFramePrepared = true
	s.localFramePreparedCount = s.frameCounter
	s.replayManager.RecordLocalFrame(s.localFrameInputs)
}
func (s *System) preparedLocalControllerInput(controller int) (ControllerFrameInput, bool) {
	if !s.localFramePrepared || s.localFramePreparedCount != s.frameCounter {
		return ControllerFrameInput{}, false
	}
	if controller < 0 || controller >= len(s.localFrameInputs) {
		return ControllerFrameInput{}, false
	}
	return s.localFrameInputs[controller], true
}
func (s *System) clearPreparedLocalControllerFrame() {
	s.localFramePrepared = false
}
