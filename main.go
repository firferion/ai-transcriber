package main

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
)

//go:embed bin/*
var binFiles embed.FS

//go:embed winres/icon.png
var iconBytes []byte

var (
	inputPath  string
	outputPath string
	isGpu      = true
	processing = false
	currentCmd *exec.Cmd
	mu         sync.Mutex
	progress   float64
	statusMsg  = "Готов к работе"
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func getDuration(ffprobe, file string) float64 {
	cmd := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", file)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0.1
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0.1
	}
	return d
}

func extractBinaries(targetDir string) {
	os.MkdirAll(filepath.Join(targetDir, "bin"), 0755)
	files, err := binFiles.ReadDir("bin")
	if err != nil { return }
	for _, f := range files {
		if f.IsDir() { continue }
		srcPath := "bin/" + f.Name()
		dstPath := filepath.Join(targetDir, "bin", f.Name())
		
		src, err := binFiles.Open(srcPath)
		if err != nil { continue }
		
		if stat, err := os.Stat(dstPath); err == nil {
			srcStat, _ := src.Stat()
			if stat.Size() == srcStat.Size() {
				src.Close()
				continue
			}
		}
		
		dst, err := os.Create(dstPath)
		if err != nil {
			src.Close()
			continue
		}
		io.Copy(dst, src)
		src.Close()
		dst.Close()
	}
}

func main() {
	tempDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "AI_Transcriber")
	extractBinaries(tempDir)

	myApp := app.New()
	myWindow := myApp.NewWindow("AI Transcriber")
	myWindow.Resize(fyne.NewSize(500, 450))

	if res := fyne.NewStaticResource("icon.png", iconBytes); res != nil {
		myWindow.SetIcon(res)
		myApp.SetIcon(res)
	}

	lblInput := widget.NewLabel("Файл: не выбран")
	btnInput := widget.NewButton("Выбрать видео...", func() {
		fd := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err == nil && reader != nil {
				inputPath = reader.URI().Path()
				lblInput.SetText("Файл: " + filepath.Base(inputPath))
			}
		}, myWindow)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".mp4", ".mkv", ".avi", ".mov", ".webm"}))
		fd.Show()
	})

	lblOutput := widget.NewLabel("Сохранить в: не выбрано")
	btnOutput := widget.NewButton("Выбрать папку...", func() {
		dialog.ShowFolderOpen(func(lu fyne.ListableURI, err error) {
			if err == nil && lu != nil {
				outputPath = lu.Path()
				lblOutput.SetText("Сохранить в: " + outputPath)
			}
		}, myWindow)
	})

	lblDevice := widget.NewLabel("Устройство обработки:")
	radioDevice := widget.NewRadioGroup([]string{"NVIDIA (GPU)", "Процессор (CPU)"}, func(s string) {})
	radioDevice.Horizontal = true
	radioDevice.SetSelected("NVIDIA (GPU)")

	pBar := widget.NewProgressBar()
	lblStatus := widget.NewLabel("Готов к работе")
	lblStatus.Alignment = fyne.TextAlignCenter

	var btnStart, btnStop *widget.Button

	updateUI := func(p float64, s string, proc bool) {
		pBar.SetValue(p)
		lblStatus.SetText(s)
		if proc {
			btnStart.Disable()
			btnStop.Enable()
		} else {
			btnStart.Enable()
			btnStop.Disable()
		}
	}

	btnStart = widget.NewButton("ЗАПУСТИТЬ", func() {
		if inputPath == "" || outputPath == "" {
			dialog.ShowInformation("Ошибка", "Выберите видео и папку для сохранения", myWindow)
			return
		}

		mu.Lock()
		processing = true
		isGpu = radioDevice.Selected == "NVIDIA (GPU)"
		progress = 0.0
		statusMsg = "Подготовка..."
		mu.Unlock()

		updateUI(0.0, statusMsg, true)

		go func() {
			binDir := filepath.Join(tempDir, "bin")
			ffmpeg := filepath.Join(binDir, "ffmpeg.exe")
			ffprobe := filepath.Join(binDir, "ffprobe.exe")
			whisper := filepath.Join(binDir, "whisper-cli.exe")
			model := filepath.Join(binDir, "ggml-large-v3-turbo-q5_0.bin")

			tempWav := filepath.Join(os.TempDir(), "transcribe_temp.wav")
			os.Remove(tempWav)

			totalDur := getDuration(ffprobe, inputPath)

			// FFmpeg 0-15%
			cmd1 := exec.Command(ffmpeg, "-i", inputPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-progress", "-", "-nostats", "-y", tempWav)
			hideWindow(cmd1)
			
			mu.Lock()
			currentCmd = cmd1
			mu.Unlock()
			
			stdout1, _ := cmd1.StdoutPipe()
			cmd1.Start()

			scanner1 := bufio.NewScanner(stdout1)
			for scanner1.Scan() {
				line := scanner1.Text()
				if strings.HasPrefix(line, "out_time_ms=") {
					usStr := strings.TrimPrefix(line, "out_time_ms=")
					us, err := strconv.ParseFloat(usStr, 64)
					if err == nil {
						sec := us / 1000000.0
						pct := (sec / totalDur) * 0.15
						if pct > 0.15 { pct = 0.15 }
						updateUI(pct, fmt.Sprintf("Конвертация аудио: %d%%", int((sec/totalDur)*100.0)), true)
					}
				}
			}
			cmd1.Wait()

			// Whisper 15-100%
			outBase := filepath.Join(outputPath, strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath)))
			args := []string{"-m", model, "-f", tempWav, "-l", "ru", "-otxt", "-of", outBase, "-pp"}
			if !isGpu {
				args = append(args, "--no-gpu")
			}

			cmd2 := exec.Command(whisper, args...)
			hideWindow(cmd2)
			
			mu.Lock()
			currentCmd = cmd2
			mu.Unlock()

			stderr2, _ := cmd2.StderrPipe()
			cmd2.Start()

			scanner2 := bufio.NewScanner(stderr2)
			for scanner2.Scan() {
				line := scanner2.Text()
				if strings.Contains(line, "progress =") {
					parts := strings.Split(line, "progress =")
					if len(parts) > 1 {
						pStr := strings.TrimSpace(parts[1])
						pStr = strings.TrimSuffix(pStr, "%")
						var p int
						fmt.Sscanf(pStr, "%d", &p)
						scaled := 0.15 + (float64(p) * 0.0085)
						updateUI(scaled, fmt.Sprintf("Транскрибация: %d%%", p), true)
					}
				}
			}
			cmd2.Wait()

			mu.Lock()
			processing = false
			currentCmd = nil
			mu.Unlock()

			updateUI(1.0, "Завершено!", false)
		}()
	})
	btnStart.Importance = widget.HighImportance

	btnStop = widget.NewButton("СТОП", func() {
		mu.Lock()
		if currentCmd != nil && currentCmd.Process != nil {
			currentCmd.Process.Kill()
		}
		processing = false
		mu.Unlock()
		updateUI(0.0, "Остановлено", false)
	})
	btnStop.Disable()

	content := container.NewVBox(
		lblInput,
		btnInput,
		widget.NewSeparator(),
		lblOutput,
		btnOutput,
		widget.NewSeparator(),
		lblDevice,
		radioDevice,
		widget.NewSeparator(),
		pBar,
		lblStatus,
		container.NewGridWithColumns(2, btnStart, btnStop),
	)

	myWindow.SetContent(container.NewPadded(content))
	myWindow.ShowAndRun()
}
