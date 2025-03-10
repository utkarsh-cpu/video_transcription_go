package main

// #cgo pkg-config: tesseract
import "C"

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

// ChunkData struct
type ChunkData struct {
	VideoPath  string
	AudioPath  string
	ChunkNum   int
	Err        error
	VideoIndex int
	BaseName   string
}

// setLlmApi function
func setLlmApi(llm string, apiKey string) (*genai.Client, *genai.GenerativeModel, context.Context) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatal(err)
	}
	model := client.GenerativeModel(llm)
	fmt.Println("LLM API setup complete.")
	return client, model, ctx
}

const (
	maxRetries = 3                // Maximum number of retry attempts for LLM calls
	retryDelay = 15 * time.Second // Delay between retry attempts
)

// sentLlmPrompt function
func sentLlmPrompt(model *genai.GenerativeModel, prompt []genai.Part, ctx context.Context, file *os.File, videoIndex int) string {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		fmt.Printf("Sending combined prompt for video %d to LLM, attempt %d...\n", videoIndex, attempt+1)
		startTime := time.Now()
		resp, err := model.GenerateContent(ctx, prompt...)
		if err == nil {
			duration := time.Since(startTime)
			fmt.Printf("LLM response received for video %d in %v.\n", videoIndex, duration)
			var llmResponse string
			for _, c := range resp.Candidates {
				if c.Content != nil {
					for _, part := range c.Content.Parts {
						if text, ok := part.(genai.Text); ok {
							llmResponse += string(text)
							if file != nil { // Check if file is nil before writing
								if _, err := fmt.Fprintln(file, string(text)); err != nil {
									log.Println("Error writing to file:", err)
								}
							}
						}
					}
				}
			}
			fmt.Printf("Combined prompt processed and written to file for video %d.\n", videoIndex)
			return llmResponse
		}

		log.Printf("Error generating content for video %d (attempt %d): %v\n", videoIndex, attempt+1, err)
		if attempt < maxRetries {
			fmt.Printf("Retrying in %v...\n", retryDelay)
			time.Sleep(retryDelay)
		} else {
			fmt.Printf("Max retries reached for video %d. Aborting LLM call.\n", videoIndex)
			return "" // Return empty string if max retries reached
		}
	}
	return "" // Should not reach here, but added for completeness
}

// chunkVideo function
func chunkVideo(videoPath string, chunkDuration int, videoIndex int, baseName string) ([]ChunkData, error) {
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "video_chunks")
	if err != nil {
		return nil, fmt.Errorf("error creating temporary directory: %w", err)
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", videoPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("error getting video duration: %w, output: %s", err, string(output))
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("error parsing video duration: %w", err)
	}

	numChunks := int(duration / float64(chunkDuration))
	if int(duration)%chunkDuration != 0 {
		numChunks++
	}

	var chunks []ChunkData

	for i := 0; i < numChunks; i++ {
		startTime := i * chunkDuration
		chunkVideoPath := fmt.Sprintf("%s/chunk_%d_video_%d.mp4", tempDir, i, videoIndex)
		chunkAudioPath := fmt.Sprintf("%s/chunk_%d_video_%d.wav", tempDir, i, videoIndex)

		cmd := exec.Command("ffmpeg",
			"-ss", fmt.Sprintf("%d", startTime),
			"-i", videoPath,
			"-t", fmt.Sprintf("%d", chunkDuration),
			"-c", "copy",
			"-an", chunkVideoPath,
			"-ss", fmt.Sprintf("%d", startTime),
			"-i", videoPath,
			"-t", fmt.Sprintf("%d", chunkDuration),
			"-vn",
			"-acodec", "pcm_s16le", // 16-bit WAV audio
			chunkAudioPath,
		)

		output, err = cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("error creating video chunk %d for video %d: %w, output: %s", i, videoIndex, err, string(output))
		}
		chunks = append(chunks, ChunkData{VideoPath: chunkVideoPath, AudioPath: chunkAudioPath, ChunkNum: i, VideoIndex: videoIndex, BaseName: baseName})
	}

	return chunks, nil
}

// transcribeAudioWhisperCLI function
func transcribeAudioWhisperCLI(audioPath string, whisperCLIPath string, whisperModelPath string, videoIndex int, chunkNum int, threads int, language string) (string, error) {
	cmdArgs := []string{
		"--model", whisperModelPath,
		"--threads", fmt.Sprintf("%d", threads),
	}
	if language != "" {
		cmdArgs = append(cmdArgs, "--language", language)
	}
	cmdArgs = append(cmdArgs, audioPath)

	cmd := exec.Command(whisperCLIPath, cmdArgs...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	fmt.Printf("Starting whisper-cli for video %d chunk %d, Audio Path: %s\n", videoIndex, chunkNum, audioPath)
	startTime := time.Now()

	err := cmd.Run()
	duration := time.Since(startTime)
	fmt.Printf("Whisper-cli finished for video %d chunk %d in %v\n", videoIndex, chunkNum, duration)

	if err != nil {
		return "", fmt.Errorf("error running whisper-cli for video %d chunk %d: %w, stderr: %s", videoIndex, chunkNum, err, stderr.String())
	}

	transcript := out.String()
	return transcript, nil
}

// extractFrames function
func extractFrames(videoPath string, videoIndex int, chunkNum int) ([]string, error) {
	tempDir, err := os.MkdirTemp("", fmt.Sprintf("frames_video%d_chunk%d", videoIndex, chunkNum))
	if err != nil {
		return nil, fmt.Errorf("error creating temporary directory for frames: %w", err)
	}

	// Extract frames at 1fps.  Adjust -r as needed.
	cmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-r", "1", // Frames per second
		"-q:v", "2", // JPEG quality (2 is high)
		fmt.Sprintf("%s/frame_%%04d.jpg", tempDir),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("error extracting frames: %w, output: %s", err, string(output))
	}

	// Get list of extracted frame files
	var framePaths []string
	filepath.WalkDir(tempDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jpg") {
			framePaths = append(framePaths, path)
		}
		return nil
	})

	return framePaths, nil
}

// transcribeFramesTesseractCLI function
func transcribeFramesTesseractCLI(framePaths []string) (string, error) {
	var combinedTranscript strings.Builder
	var wg sync.WaitGroup
	frameResults := make(chan struct {
		Text  string
		Error error
	}, len(framePaths)) // Buffered channel for results

	// Limit concurrency to the number of CPUs (or a reasonable limit)
	numWorkers := runtime.NumCPU()
	if numWorkers > 8 { //  cap it to 8 for now to avoid too many subprocesses
		numWorkers = 8
	}
	guard := make(chan struct{}, numWorkers) // Semaphore

	for _, framePath := range framePaths {
		wg.Add(1)
		guard <- struct{}{} // Acquire a slot

		go func(fp string) {
			defer wg.Done()
			defer func() { <-guard }() // Release the slot

			// Open the image file
			imgFile, err := os.Open(fp)
			if err != nil {
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error opening image file %s: %w", fp, err)}
				return
			}

			// Decode the image
			img, _, err := image.Decode(imgFile)
			imgFile.Close() // Close immediately after decoding
			if err != nil {
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error decoding image file %s: %w", fp, err)}
				return
			}

			// Convert to JPEG
			buf := new(bytes.Buffer)
			if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 90}); err != nil {
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error encoding image to JPEG: %w", err)}
				return
			}
			jpegBytes := buf.Bytes()

			tempFile, err := os.CreateTemp("", "ocr_*.jpg")
			if err != nil {
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error creating temp file: %w", err)}
				return
			}
			tempFilePath := tempFile.Name()
			defer os.Remove(tempFilePath)

			_, err = tempFile.Write(jpegBytes)
			if err != nil {
				tempFile.Close() // Close before removing
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error writing to temp file: %w", err)}
				return
			}
			if err := tempFile.Close(); err != nil {
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error closing temp file: %w", err)}
				return
			}

			cmd := exec.Command("tesseract", tempFilePath, "stdout")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err = cmd.Run()
			if err != nil {
				frameResults <- struct {
					Text  string
					Error error
				}{"", fmt.Errorf("error running tesseract on %s: %w, stderr: %s", fp, err, stderr.String())}
				return
			}

			frameResults <- struct {
				Text  string
				Error error
			}{stdout.String(), nil}

		}(framePath)
	}

	wg.Wait()           // Wait for all goroutines to finish
	close(frameResults) // Close the channel - no more results coming

	// Collect results from the channel
	for result := range frameResults {
		if result.Error != nil {
			log.Println(result.Error) // Log individual errors
			continue                  // Skip frames with errors
		}
		combinedTranscript.WriteString(result.Text)
		combinedTranscript.WriteString("\n")
	}

	return combinedTranscript.String(), nil
}

// transcribeVideoLLM function
func transcribeVideoLLM(ctx context.Context, client *genai.Client, model *genai.GenerativeModel, videoPath string, videoIndex int, chunkNum int) (string, error) {
	uploadedFile, err := client.UploadFileFromPath(ctx, videoPath, nil)
	if err != nil {
		// If LLM fails, fall back to Tesseract
		fmt.Printf("Chunk %d for video %d: LLM upload failed, falling back to Tesseract...\n", chunkNum, videoIndex)
		framePaths, err := extractFrames(videoPath, videoIndex, chunkNum)
		if err != nil {
			return "", fmt.Errorf("error extracting frames for video %d chunk %d: %w", videoIndex, chunkNum, err)
		}
		transcript, err := transcribeFramesTesseractCLI(framePaths)
		if err != nil {
			return "", fmt.Errorf("error transcribing frames with Tesseract for video %d chunk %d: %w", videoIndex, chunkNum, err)
		}

		// Cleanup extracted frames.
		if len(framePaths) > 0 {
			os.RemoveAll(filepath.Dir(framePaths[0]))
		}
		return transcript, nil

	}

	fmt.Println("Waiting for 30 seconds after file upload to ensure file activation...")
	time.Sleep(30 * time.Second) // Wait for file to be ready
	defer func() { client.DeleteFile(ctx, uploadedFile.Name) }()

	fmt.Printf("Chunk %d for video %d: Video chunk uploaded as: %s\n", chunkNum, videoIndex, uploadedFile.URI)

	promptList := []genai.Part{
		genai.Text("## Task Description\nAnalyze the video and provide a detailed raw transcription of text displayed in the video."),
		genai.FileData{URI: uploadedFile.URI},
	}
	videoTranscript := sentLlmPrompt(model, promptList, ctx, nil, videoIndex) // No file writing here

	if videoTranscript == "" {
		// If LLM transcription fails, fall back to Tesseract
		fmt.Printf("Chunk %d for video %d: LLM transcription failed, falling back to Tesseract...\n", chunkNum, videoIndex)
		framePaths, err := extractFrames(videoPath, videoIndex, chunkNum)
		if err != nil {
			return "", fmt.Errorf("error extracting frames for video %d chunk %d: %w", videoIndex, chunkNum, err)
		}
		transcript, err := transcribeFramesTesseractCLI(framePaths)
		// Cleanup extracted frames.
		if len(framePaths) > 0 {
			os.RemoveAll(filepath.Dir(framePaths[0]))
		}
		if err != nil {
			return "", fmt.Errorf("error transcribing frames with Tesseract for video %d chunk %d: %w", videoIndex, chunkNum, err)
		}
		return transcript, nil
	}

	fmt.Printf("Chunk %d for video %d: Video transcribed by LLM.\n", chunkNum, videoIndex)

	return videoTranscript, nil
}

// processChunk function
func processChunk(chunkData ChunkData, client *genai.Client, model *genai.GenerativeModel, ctx context.Context, errorChannel chan<- error, whisperCLIPath string, whisperModelPath string, whisperThreads int, whisperLanguage string, audioOutputFile, videoOutputFile *os.File) {
	chunk := chunkData

	if chunk.Err != nil {
		errorChannel <- chunk.Err
		return
	}

	fmt.Printf("Processing chunk %d for video %d...\n", chunk.ChunkNum, chunk.VideoIndex)
	defer fmt.Printf("Finished processing chunk %d for video %d.\n", chunk.ChunkNum, chunk.VideoIndex)

	var wg sync.WaitGroup
	wg.Add(2) // We have two goroutines: audio and video transcription

	var audioTranscript string
	var audioErr error
	go func() {
		defer wg.Done()
		audioTranscript, audioErr = transcribeAudioWhisperCLI(chunk.AudioPath, whisperCLIPath, whisperModelPath, chunk.VideoIndex, chunk.ChunkNum, whisperThreads, whisperLanguage)
		if audioErr != nil {
			errorChannel <- fmt.Errorf("error transcribing audio for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, audioErr)
			audioTranscript = fmt.Sprintf("Audio transcription failed for video %d chunk %d.", chunk.VideoIndex, chunk.ChunkNum)
		}
		// Write to audio output file *immediately*
		_, err := fmt.Fprintf(audioOutputFile, "Video Index: %d, Chunk: %d\n%s\n", chunk.VideoIndex, chunk.ChunkNum, audioTranscript)
		if err != nil {
			errorChannel <- fmt.Errorf("error writing to audio file for video %d chunk %d: %v", chunk.VideoIndex, chunk.ChunkNum, err)
		}
		fmt.Printf("Chunk %d for video %d: Audio transcribed and written to audio output file.\n", chunk.ChunkNum, chunk.VideoIndex)
		os.Remove(chunk.AudioPath) // Delete audio chunk
	}()

	var videoTranscript string
	var videoErr error
	go func() {
		defer wg.Done()
		videoTranscript, videoErr = transcribeVideoLLM(ctx, client, model, chunk.VideoPath, chunk.VideoIndex, chunk.ChunkNum)
		if videoErr != nil {
			errorChannel <- fmt.Errorf("error transcribing video for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, videoErr)
			videoTranscript = fmt.Sprintf("Video transcription failed for video %d chunk %d.", chunk.VideoIndex, chunk.ChunkNum)
		}
		// Write to video output file *immediately*
		_, err := fmt.Fprintf(videoOutputFile, "Video Index: %d, Chunk: %d\n%s\n", chunk.VideoIndex, chunk.ChunkNum, videoTranscript)
		if err != nil {
			errorChannel <- fmt.Errorf("error writing to video file for video %d chunk %d: %v", chunk.VideoIndex, chunk.ChunkNum, err)
		}
		fmt.Printf("Chunk %d for video %d: Video transcribed and written to video output file.\n", chunk.ChunkNum, chunk.VideoIndex)
		os.Remove(chunk.VideoPath) // Delete video chunk
	}()

	wg.Wait() // Wait for both goroutines to complete

}

// main function
func main() {
	runtime.GOMAXPROCS(runtime.NumCPU()) // Use all available CPUs

	if len(os.Args) != 9 {
		fmt.Println("Usage: program <llm_model> <api_key> <chunk_duration_seconds> <whisper_cli_path> <whisper_model_path> <whisper_threads> <whisper_language> <video_path_or_folder>")
		os.Exit(1)
	}
	llm := os.Args[1]
	apiKey := os.Args[2]
	chunkDuration, err := strconv.Atoi(os.Args[3])
	if err != nil {
		log.Fatalf("Invalid chunk duration: %v\n", err)
	}
	whisperCLIPath := os.Args[4]
	whisperModelPath := os.Args[5]
	whisperThreads, err := strconv.Atoi(os.Args[6])
	if err != nil {
		log.Fatalf("Invalid whisper threads: %v\n", err)
	}
	whisperLanguage := os.Args[7]
	inputPath := os.Args[8]

	client, model, ctx := setLlmApi(llm, apiKey)
	defer client.Close()

	errorChannel := make(chan error, 10) // Buffered channel

	var videoPaths []string
	fileInfo, err := os.Stat(inputPath)
	if err != nil {
		log.Fatalf("Error accessing input path: %v\n", err)
	}

	if fileInfo.IsDir() {
		fmt.Println("Processing folder:", inputPath)
		err = filepath.WalkDir(inputPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && isVideoFile(path) {
				videoPaths = append(videoPaths, path)
			}
			return nil
		})
		if err != nil {
			log.Fatalf("Error walking directory: %v\n", err)
		}
	} else {
		fmt.Println("Processing single file:", inputPath)
		if isVideoFile(inputPath) {
			videoPaths = append(videoPaths, inputPath)
		} else {
			log.Println("Warning: Input path is not a video file:", inputPath)
		}
	}

	if len(videoPaths) == 0 {
		fmt.Println("No video files found to process.")
		return
	}

	for videoIndex, videoPath := range videoPaths {
		baseName := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
		outputFileName := baseName + "_output.txt"
		audioOutputFileName := baseName + "_audio_output.txt"
		videoOutputFileName := baseName + "_video_output.txt"

		fmt.Printf("\n--- START PROCESSING VIDEO %d: %s ---\n", videoIndex+1, videoPath)
		fmt.Println("Creating output files for video:", videoPath)

		outputFile, err := os.Create(outputFileName)
		if err != nil {
			log.Fatalf("Error creating output file for video %s: %v\n", videoPath, err)
			continue // Continue to the next video
		}
		defer outputFile.Close()

		audioOutputFile, err := os.Create(audioOutputFileName)
		if err != nil {
			log.Fatalf("Error creating audio output file for video %s: %v\n", videoPath, err)
			continue
		}
		defer audioOutputFile.Close()

		videoOutputFile, err := os.Create(videoOutputFileName)
		if err != nil {
			log.Fatalf("Error creating video output file for video %s: %v\n", videoPath, err)
			continue
		}
		defer videoOutputFile.Close()
		fmt.Println("Output files created for video:", videoPath)

		fmt.Println("Chunking video sequentially...")
		chunks, err := chunkVideo(videoPath, chunkDuration, videoIndex+1, baseName)
		if err != nil {
			log.Printf("Error chunking video %s: %v\n", videoPath, err)
			continue
		}
		fmt.Println("Video chunking complete.")

		fmt.Println("Processing video chunks in parallel...")
		// No more slices needed here

		for _, chunkData := range chunks {
			processChunk(chunkData, client, model, ctx, errorChannel, whisperCLIPath, whisperModelPath, whisperThreads, whisperLanguage, audioOutputFile, videoOutputFile)
		}

		fmt.Println("All video chunks processed. Sending combined prompt to LLM...")

		// Read the *entire* content of the audio and video files.
		audioContent, err := os.ReadFile(audioOutputFileName)
		if err != nil {
			log.Printf("Error reading audio output file: %v", err)
			continue // Crucial: Continue to the next video if reading fails
		}
		videoContent, err := os.ReadFile(videoOutputFileName)
		if err != nil {
			log.Printf("Error reading video output file: %v", err)
			continue
		}
		combinedAudioTranscript := string(audioContent) // Convert to string
		combinedVideoTranscript := string(videoContent)

		combinedPromptText := fmt.Sprintf(`Here is a raw transcription of a video. Your task is to refine it into a well-structured, human-like summary with explanations while keeping all the original details. Analyze the lecture provided in the audio transcription and video text.  Identify the main topic, key arguments, supporting evidence, and any examples used.  Explain the lecture in a structured way, highlighting the connections between different ideas.  Use information from both the audio transcription and video text to create a comprehensive explanation, also use timestamp to help us correlate with the audio transcript:

    --- RAW TRANSCRIPTION of Audio ---
    %s

    --- RAW TRANSCRIPTION of Video Text ---
    %s

    Please rewrite it clearly with explanations where needed, ensuring it's easy to read and understand.`, combinedAudioTranscript, combinedVideoTranscript)

		combinedPrompt := []genai.Part{
			genai.Text(combinedPromptText),
		}

		sentLlmPrompt(model, combinedPrompt, ctx, outputFile, videoIndex+1) // Now passing the file
		fmt.Printf("\n--- FINISHED PROCESSING VIDEO %d: %s ---\n", videoIndex+1, videoPath)
		fmt.Fprintf(outputFile, "\n--- VIDEO %d PROCESSING COMPLETE ---\n\n", videoIndex+1)
		fmt.Fprintf(audioOutputFile, "\n--- VIDEO %d PROCESSING COMPLETE ---\n\n", videoIndex+1)
		fmt.Fprintf(videoOutputFile, "\n--- VIDEO %d PROCESSING COMPLETE ---\n\n", videoIndex+1)
	}
	close(errorChannel) // Close *after* the loop, *before* reading
	for err := range errorChannel {
		log.Println("Error from goroutine:", err)
	}

	fmt.Println("\nAll videos processing complete.")
	fmt.Println("Exiting.")
}

// isVideoFile function
func isVideoFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	videoExtensions := []string{".mp4", ".mov", ".avi", ".wmv", ".mkv", ".flv", ".webm", ".mpeg", ".mpg"}
	for _, vext := range videoExtensions {
		if ext == vext {
			return true
		}
	}
	return false
}

/*
package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChunkData struct to hold info about a video chunk
type ChunkData struct {
	VideoPath  string
	AudioPath  string
	ChunkNum   int
	Err        error  // To hold any error during chunking
	VideoIndex int    // To identify which video the chunk belongs to
	BaseName   string // Base filename for output files
}

func setLlmApi(llm string, apiKey string) (*genai.Client, *genai.GenerativeModel, context.Context) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatal(err)
	}
	model := client.GenerativeModel(llm)
	fmt.Println("LLM API setup complete.")
	return client, model, ctx
}

// deletionWorker reads genai.File from the deleteChannel and deletes the files.
func deletionWorker(client *genai.Client, ctx context.Context, deleteChannel <-chan genai.File, wg *sync.WaitGroup) { // Expecting genai.File now
	defer wg.Done()
	fmt.Println("Deletion worker started.")
	defer fmt.Println("Deletion worker finished.")
	for file := range deleteChannel { // Receiving genai.File
		err := client.DeleteFile(ctx, file.Name) // Use file.Name for deletion
		if err != nil {
			log.Printf("Error deleting file %s (Name: %s, URI: %s): %v\n", file.URI, file.Name, file.URI, err) // Log both URI and Name for clarity
		} else {
			fmt.Printf("Deleted file (Name: %s, URI: %s)\n", file.Name, file.URI) // Indicate successful deletion with Name and URI
		}
	}
}

func sentLlmPrompt(model *genai.GenerativeModel, prompt []genai.Part, ctx context.Context, file *os.File, batchNum int, videoIndex int) string { // Added videoIndex
	fmt.Printf("Sending batch %d for video %d to LLM...\n", batchNum, videoIndex) // Indicate video index
	startTime := time.Now()
	resp, err := model.GenerateContent(ctx, prompt...)
	if err != nil {
		log.Println("Error generating content:", err)
		return "" // Return empty string on error
	}
	duration := time.Since(startTime)
	fmt.Printf("LLM response received for video %d in %v.\n", videoIndex, duration) // Indicate video index
	var llmResponse string
	for _, c := range resp.Candidates {
		if c.Content != nil {
			for _, part := range c.Content.Parts {
				if text, ok := part.(genai.Text); ok {
					llmResponse += string(text) // Append to response string
					if _, err := fmt.Fprintln(file, string(text)); err != nil {
						log.Println("Error writing to file:", err)
					}
				}
			}
		}
	}
	fmt.Printf("Batch %d for video %d processed and written to file.\n", batchNum, videoIndex) // Indicate video index
	return llmResponse                                                                         // Return the full response string
}

// getWorkerCounts determines the number of deletion workers based on system CPU cores and environment variables.
func getWorkerCounts() int {
	numCPU := runtime.NumCPU()
	fmt.Printf("System CPU cores: %d\n", numCPU)

	var deletionWorkersPerCore int
	if envDeletionWorkersPerCore := os.Getenv("DELETION_WORKERS_PER_CORE"); envDeletionWorkersPerCore != "" {
		deletionWorkersPerCoreEnv, err := strconv.Atoi(envDeletionWorkersPerCore)
		if err != nil {
			log.Printf("Warning: Invalid DELETION_WORKERS_PER_CORE environment variable '%s', using default.\n", envDeletionWorkersPerCore)
			deletionWorkersPerCore = 2 // Default if parsing fails
		} else {
			deletionWorkersPerCore = deletionWorkersPerCoreEnv
		}
	} else {
		deletionWorkersPerCore = 2 // Default
	}

	var minDeletionWorkers int
	if envMinDeletionWorkers := os.Getenv("MIN_DELETION_WORKERS"); envMinDeletionWorkers != "" {
		minDeletionWorkersEnv, err := strconv.Atoi(envMinDeletionWorkers)
		if err != nil {
			log.Printf("Warning: Invalid MIN_DELETION_WORKERS environment variable '%s', using default.\n", envMinDeletionWorkers)
			minDeletionWorkers = 2 // Default if parsing fails
		} else {
			minDeletionWorkers = minDeletionWorkersEnv
		}
	} else {
		minDeletionWorkers = 2 // Default
	}
	var maxDeletionWorkers int
	if envMaxDeletionWorkers := os.Getenv("MAX_DELETION_WORKERS"); envMaxDeletionWorkers != "" {
		maxDeletionWorkersEnv, err := strconv.Atoi(envMaxDeletionWorkers)
		if err != nil {
			log.Printf("Warning: Invalid MAX_DELETORS_PER_CORE environment variable '%s', using default.\n", envMaxDeletionWorkers)
			maxDeletionWorkers = 10 // Default if parsing fails
		} else {
			maxDeletionWorkers = maxDeletionWorkersEnv
		}
	} else {
		maxDeletionWorkers = 10 // Default
	}

	numDeletionWorkersCalculated := numCPU * deletionWorkersPerCore
	numDeletionWorkers := numDeletionWorkersCalculated
	if numDeletionWorkers < minDeletionWorkers {
		numDeletionWorkers = minDeletionWorkers
	}
	if numDeletionWorkers > maxDeletionWorkers {
		numDeletionWorkers = maxDeletionWorkers
	}

	if envDeletionWorkers := os.Getenv("DELETION_WORKERS"); envDeletionWorkers != "" {
		deletionWorkersEnv, err := strconv.Atoi(envDeletionWorkers)
		if err != nil {
			log.Printf("Warning: Invalid DELETION_WORKERS environment variable '%s', using calculated/default.\n", envDeletionWorkers)
		} else {
			numDeletionWorkers = deletionWorkersEnv // Override calculated value
			fmt.Printf("DELETION_WORKERS environment variable set, overriding calculated deletion workers to: %d\n", numDeletionWorkers)
		}
	} else {
		fmt.Printf("Calculated number of deletion workers: %d (based on CPU cores and defaults)\n", numDeletionWorkers)
	}

	return numDeletionWorkers
}

// chunkVideo splits the video into smaller chunks in parallel.
func chunkVideo(videoPath string, chunkDuration int, numChunkWorkers int, videoIndex int, baseName string) (<-chan ChunkData, error) { // Added baseName
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "video_chunks")
	if err != nil {
		return nil, fmt.Errorf("error creating temporary directory: %w", err)
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", videoPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(tempDir) // Cleanup on error
		return nil, fmt.Errorf("error getting video duration: %w, output: %s", err, string(output))
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		os.RemoveAll(tempDir) // Cleanup on error
		return nil, fmt.Errorf("error parsing video duration: %w", err)
	}

	numChunks := int(duration / float64(chunkDuration))
	if int(duration)%chunkDuration != 0 {
		numChunks++
	}

	chunksChan := make(chan ChunkData, numChunks)           // Buffered channel for chunks
	chunkWorkerPool := make(chan struct{}, numChunkWorkers) // Worker pool semaphore
	var wg sync.WaitGroup

	for i := 0; i < numChunks; i++ {
		chunkWorkerPool <- struct{}{} // Acquire worker slot
		wg.Add(1)
		go func(chunkNum int, videoIndex int, baseName string) { // Pass baseName
			defer func() {
				<-chunkWorkerPool // Release worker slot
				wg.Done()
			}()

			startTime := chunkNum * chunkDuration
			chunkVideoPath := fmt.Sprintf("%s/chunk_%d_video_%d.mp4", tempDir, chunkNum, videoIndex) // Unique chunk names
			chunkAudioPath := fmt.Sprintf("%s/chunk_%d_video_%d.aac", tempDir, chunkNum, videoIndex) // Unique chunk names

			cmd := exec.Command("ffmpeg",
				"-ss", fmt.Sprintf("%d", startTime),
				"-i", videoPath,
				"-t", fmt.Sprintf("%d", chunkDuration),
				"-c", "copy",
				"-an", chunkVideoPath,
				"-ss", fmt.Sprintf("%d", startTime),
				"-i", videoPath,
				"-t", fmt.Sprintf("%d", chunkDuration),
				"-vn",
				"-acodec", "copy", chunkAudioPath,
			)

			output, err := cmd.CombinedOutput()
			if err != nil {
				chunksChan <- ChunkData{ChunkNum: chunkNum, Err: fmt.Errorf("error creating video chunk %d for video %d: %w, output: %s", chunkNum, videoIndex, err, string(output)), VideoIndex: videoIndex, BaseName: baseName} // Pass baseName
				return
			}
			chunksChan <- ChunkData{VideoPath: chunkVideoPath, AudioPath: chunkAudioPath, ChunkNum: chunkNum, VideoIndex: videoIndex, BaseName: baseName} // Pass baseName
		}(i, videoIndex, baseName) // Pass baseName to goroutine
	}

	go func() {
		wg.Wait()
		close(chunksChan)
	}()

	return chunksChan, nil
}

func transcribeAudioWhisperCLI(audioPath string, whisperCLIPath string, whisperModelPath string, videoIndex int, chunkNum int, threads int, language string) (string, error, string) { // Removed workerPool
	outputFilename := filepath.Join(filepath.Dir(audioPath), fmt.Sprintf("%s_chunk_%d.txt", strings.TrimSuffix(filepath.Base(audioPath), filepath.Ext(audioPath)), chunkNum)) // Create output filename
	cmdArgs := []string{
		"--model", whisperModelPath,
		"--threads", fmt.Sprintf("%d", threads), // Add threads option
		"--output-txt",                                              // Enable text output
		"--output-file", strings.TrimSuffix(outputFilename, ".txt"), // Output file without extension
	}
	if language != "" {
		cmdArgs = append(cmdArgs, "--language", language) // Add language option if specified
	}
	cmdArgs = append(cmdArgs, audioPath)

	cmd := exec.Command(whisperCLIPath, cmdArgs...) // Use provided path and model path with options
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	fmt.Printf("Starting whisper-cli for video %d chunk %d, Audio Path: %s\n", videoIndex, chunkNum, audioPath) //  LOGGING - Whisper start
	startTime := time.Now()                                                                                     // Start timer

	err := cmd.Run()
	duration := time.Since(startTime)                                                                                                 // End timer
	fmt.Printf("Whisper-cli finished for video %d chunk %d in %v, Output File: %s\n", videoIndex, chunkNum, duration, outputFilename) //  LOGGING - Whisper finish

	if err != nil {
		return "", fmt.Errorf("error running whisper-cli for video %d chunk %d: %w, stderr: %s", videoIndex, chunkNum, err, stderr.String()), "" // Return empty transcript and filename, error
	}

	transcriptBytes, err := os.ReadFile(outputFilename) // Read transcript from file
	if err != nil {
		return "", fmt.Errorf("error reading transcript file: %w", err), "" // Return empty transcript and filename, error
	}
	transcript := string(transcriptBytes)

	return transcript, nil, outputFilename // Return transcript, nil error, and outputFilename
}

func processChunk(chunkData ChunkData, llm string, apiKey string, outputFileName string, audioOutputFileName string, videoOutputFileName string, client *genai.Client, model *genai.GenerativeModel, ctx context.Context, errorChannel chan<- error, wg *sync.WaitGroup, whisperCLIPath string, whisperModelPath string, whisperThreads int, whisperLanguage string) { // Removed whisperWorkerPool
	defer wg.Done()
	chunk := chunkData // Rename for clarity

	if chunk.Err != nil {
		errorChannel <- chunk.Err // Report chunking error
		return
	}

	fmt.Printf("Processing chunk %d for video %d...\n", chunk.ChunkNum, chunk.VideoIndex)              // Indicate video index
	defer fmt.Printf("Finished processing chunk %d for video %d.\n", chunk.ChunkNum, chunk.VideoIndex) // Indicate video index

	// Open main output file (append mode) - Filename is now passed in ChunkData
	outputFile, err := os.OpenFile(outputFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		errorChannel <- fmt.Errorf("error opening output file: %w", err)
		return
	}
	defer func() {
		if err := outputFile.Close(); err != nil {
			errorChannel <- fmt.Errorf("error closing output file: %w", err)
		}
	}()

	// Open audio output file (append mode) - Filename is now passed in ChunkData
	audioOutputFile, err := os.OpenFile(audioOutputFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		errorChannel <- fmt.Errorf("error opening audio output file: %w", err)
		return
	}
	defer func() {
		if err := audioOutputFile.Close(); err != nil {
			errorChannel <- fmt.Errorf("error closing audio output file: %w", err)
		}
	}()

	// Open video output file (append mode) - Filename is now passed in ChunkData
	videoOutputFile, err := os.OpenFile(videoOutputFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		errorChannel <- fmt.Errorf("error opening video output file: %w", err)
		return
	}
	defer func() {
		if err := videoOutputFile.Close(); err != nil {
			errorChannel <- fmt.Errorf("error closing video output file: %w", err)
		}
	}()

	// 1. Audio Transcription using whisper-cli (Sequential Now)
	var audioTranscript string
	var whisperErr error
	var whisperOutputFilename string                                                                                                                                                                     // Capture output filename
	audioTranscript, whisperErr, whisperOutputFilename = transcribeAudioWhisperCLI(chunk.AudioPath, whisperCLIPath, whisperModelPath, chunk.VideoIndex, chunk.ChunkNum, whisperThreads, whisperLanguage) // Removed workerPool

	fmt.Printf("Audio transcription for chunk %d of video %d finished. Proceeding to video analysis.\n", chunk.ChunkNum, chunk.VideoIndex) //  LOGGING - Confirmation of wait completion

	if whisperErr != nil {
		errorChannel <- fmt.Errorf("error transcribing audio for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, whisperErr) // Indicate video and chunk info
		audioTranscript = fmt.Sprintf("Audio transcription failed for video %d chunk %d.", chunk.VideoIndex, chunk.ChunkNum)           // Include video and chunk info in error message
	}
	if _, err := fmt.Fprintf(audioOutputFile, "Video Index: %d, Chunk: %d\n%s\n", chunk.VideoIndex, chunk.ChunkNum, audioTranscript); err != nil { // Write audio transcript to file with video and chunk info
		errorChannel <- fmt.Errorf("error writing audio transcript to file: %w", err)
	}
	fmt.Printf("Chunk %d for video %d: Audio transcribed.\n", chunk.ChunkNum, chunk.VideoIndex) // Indicate video index

	// Delete audio chunk file after whisper transcription
	err = os.Remove(chunk.AudioPath)
	if err != nil {
		errorChannel <- fmt.Errorf("error deleting chunk audio file for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, err) // Indicate video and chunk info
	} else {
		fmt.Printf("Chunk %d for video %d: Audio chunk file deleted.\n", chunk.ChunkNum, chunk.VideoIndex) // Indicate video index
	}

	// Delete whisper output text file
	if whisperOutputFilename != "" {
		err = os.Remove(whisperOutputFilename)
		if err != nil {
			log.Printf("Error deleting whisper output text file %s: %v\n", whisperOutputFilename, err) // Log error, but don't fail processing
		} else {
			fmt.Printf("Chunk %d for video %d: Whisper output text file deleted: %s\n", chunk.ChunkNum, chunk.VideoIndex, whisperOutputFilename)
		}
	}

	// 2. Video Analysis with LLM - No changes to video processing part
	uploadedFile, err := client.UploadFileFromPath(ctx, chunk.VideoPath, nil)
	if err != nil {
		errorChannel <- fmt.Errorf("error uploading video chunk for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, err)
		return
	}

	fmt.Println("Waiting for 5 seconds after file upload to ensure file activation...")
	time.Sleep(5 * time.Second)

	defer func() {
		delErr := client.DeleteFile(ctx, uploadedFile.Name)
		if delErr != nil {
			errorChannel <- fmt.Errorf("error deleting uploaded video chunk from LLM for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, delErr)
		}
	}()
	fmt.Printf("Chunk %d for video %d: Video chunk uploaded as: %s\n", chunk.ChunkNum, chunk.VideoIndex, uploadedFile.URI)

	err = os.Remove(chunk.VideoPath)
	if err != nil {
		errorChannel <- fmt.Errorf("error deleting chunk video file for video %d chunk %d: %w", chunk.VideoIndex, chunk.ChunkNum, err)
	} else {
		fmt.Printf("Chunk %d for video %d: Video chunk file deleted.\n", chunk.ChunkNum, chunk.VideoIndex)
	}

	promptList := []genai.Part{
		genai.Text("## Task Description\nAnalyze the video and provide a detailed raw transcription of text displayed in the video. "),
		genai.FileData{URI: uploadedFile.URI},
	}

	videoTranscript := sentLlmPrompt(model, promptList, ctx, videoOutputFile, chunk.ChunkNum+1, chunk.VideoIndex)
	fmt.Printf("Chunk %d for video %d: Video transcribed by LLM.\n", chunk.ChunkNum, chunk.VideoIndex)

	combinedPromptText := fmt.Sprintf(`... (rest of the prompt) ...`, audioTranscript, videoTranscript) // Your prompt

	combinedPrompt := []genai.Part{
		genai.Text(combinedPromptText),
	}

	fmt.Println("Sending combined refinement prompt to LLM for video %d chunk %d...", chunk.VideoIndex, chunk.ChunkNum)
	sentLlmPrompt(model, combinedPrompt, ctx, outputFile, chunk.ChunkNum+1, chunk.VideoIndex)
	fmt.Printf("Chunk %d for video %d: Combined transcript refined by LLM.\n", chunk.ChunkNum, chunk.VideoIndex)
	fmt.Fprintf(outputFile, "\n--- VIDEO %d CHUNK %d PROCESSING COMPLETE ---\n\n", chunk.VideoIndex, chunk.ChunkNum)
}

func main() {
	if len(os.Args) != 9 { // Expecting 9 arguments now
		fmt.Println("Usage: program <llm_model> <api_key> <chunk_duration_seconds> <whisper_cli_path> <whisper_model_path> <whisper_threads> <whisper_language> <video_path_or_folder>") // Updated usage
		os.Exit(1)
	}
	llm := os.Args[1]
	apiKey := os.Args[2]
	chunkDuration, err := strconv.Atoi(os.Args[3])
	if err != nil {
		log.Fatalf("Invalid chunk duration: %v\n", err)
	}
	whisperCLIPath := os.Args[4]
	whisperModelPath := os.Args[5]
	whisperThreads, err := strconv.Atoi(os.Args[6]) // New: whisper threads
	if err != nil {
		log.Fatalf("Invalid whisper threads: %v\n", err)
	}
	whisperLanguage := os.Args[7] // New: whisper language
	inputPath := os.Args[8]       // Now inputPath can be a file or folder

	client, model, ctx := setLlmApi(llm, apiKey)
	defer func() {
		if err := client.Close(); err != nil {
			log.Println("Error closing genai client:", err)
		}
	}()

	numCPU := runtime.NumCPU()
	numChunkWorkers := numCPU / 2

	errorChannel := make(chan error, 10)
	var processWg sync.WaitGroup

	var videoPaths []string

	fileInfo, err := os.Stat(inputPath)
	if err != nil {
		log.Fatalf("Error accessing input path: %v\n", err)
	}

	if fileInfo.IsDir() {
		fmt.Println("Processing folder:", inputPath)
		filepath.WalkDir(inputPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && isVideoFile(path) {
				videoPaths = append(videoPaths, path)
			}
			return nil
		})
	} else {
		fmt.Println("Processing single file:", inputPath)
		if isVideoFile(inputPath) {
			videoPaths = append(videoPaths, inputPath)
		} else {
			log.Println("Warning: Input path is not a video file:", inputPath)
		}
	}

	if len(videoPaths) == 0 {
		fmt.Println("No video files found to process.")
		return
	}

	for videoIndex, videoPath := range videoPaths {
		baseName := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
		outputFileName := baseName + "_output.txt"
		audioOutputFileName := baseName + "_audio_output.txt"
		videoOutputFileName := baseName + "_video_output.txt"

		fmt.Printf("\n--- START PROCESSING VIDEO %d: %s ---\n", videoIndex+1, videoPath)
		fmt.Println("Creating output files for video:", videoPath)

		outputFile, err := os.Create(outputFileName)
		if err != nil {
			log.Fatalf("Error creating output file for video %s: %v\n", videoPath, err)
			continue
		}
		defer outputFile.Close()

		audioOutputFile, err := os.Create(audioOutputFileName)
		if err != nil {
			log.Fatalf("Error creating audio output file for video %s: %v\n", videoPath, err)
			continue
		}
		defer audioOutputFile.Close()

		videoOutputFile, err := os.Create(videoOutputFileName)
		if err != nil {
			log.Fatalf("Error creating video output file for video %s: %v\n", videoPath, err)
			continue
		}
		defer videoOutputFile.Close()
		fmt.Println("Output files created for video:", videoPath)

		fmt.Println("Chunking video in parallel...")
		chunksChan, err := chunkVideo(videoPath, chunkDuration, numChunkWorkers, videoIndex+1, baseName)
		if err != nil {
			log.Printf("Error chunking video %s: %v\n", videoPath, err)
			continue
		}
		fmt.Println("Video chunking started.")

		fmt.Println("Processing video chunks in parallel...")
		for chunkData := range chunksChan {
			chunkData.BaseName = baseName
			processWg.Add(1)
			go processChunk(chunkData, llm, apiKey, outputFileName, audioOutputFileName, videoOutputFileName, client, model, ctx, errorChannel, &processWg, whisperCLIPath, whisperModelPath, whisperThreads, whisperLanguage) // Removed whisperWorkerPool
		}
		processWg.Wait()
		fmt.Printf("\n--- FINISHED PROCESSING VIDEO %d: %s ---\n", videoIndex+1, videoPath)
		fmt.Fprintf(outputFile, "\n--- VIDEO %d PROCESSING COMPLETE ---\n\n", videoIndex+1)
		fmt.Fprintf(audioOutputFile, "\n--- VIDEO %d PROCESSING COMPLETE ---\n\n", videoIndex+1)
		fmt.Fprintf(videoOutputFile, "\n--- VIDEO %d PROCESSING COMPLETE ---\n\n", videoIndex+1)
		processWg.Wait() // Redundant wait, but safe
	}

	// Check for errors from goroutines
	close(errorChannel)
	for err := range errorChannel {
		log.Println("Error from goroutine:", err)
	}

	fmt.Println("\nAll videos processing complete.")
	fmt.Println("Exiting.")
}

func isVideoFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	videoExtensions := []string{".mp4", ".mov", ".avi", ".wmv", ".mkv", ".flv", ".webm", ".mpeg", ".mpg"} // Add more if needed
	for _, vext := range videoExtensions {
		if ext == vext {
			return true
		}
	}
	return false
}

/*
package main

import (
	"context"
	"fmt"
	"github.com/google/generative-ai-go/genai"
	"gocv.io/x/gocv"
	"google.golang.org/api/option"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// FrameData struct to hold both the frame and its frame number
type FrameData struct {
	Frame    gocv.Mat
	FrameNum int
}

func setLlmApi(llm string, apiKey string) (*genai.Client, *genai.GenerativeModel, context.Context) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatal(err)
	}
	model := client.GenerativeModel(llm)
	fmt.Println("LLM API setup complete.")
	return client, model, ctx
}

// deletionWorker reads genai.File from the deleteChannel and deletes the files.
func deletionWorker(client *genai.Client, ctx context.Context, deleteChannel <-chan genai.File, wg *sync.WaitGroup) { // Expecting genai.File now
	defer wg.Done()
	fmt.Println("Deletion worker started.")
	defer fmt.Println("Deletion worker finished.")
	for file := range deleteChannel { // Receiving genai.File
		err := client.DeleteFile(ctx, file.Name) // Use file.Name for deletion
		if err != nil {
			log.Printf("Error deleting file %s (Name: %s, URI: %s): %v\n", file.URI, file.Name, file.URI, err) // Log both URI and Name for clarity
		} else {
			fmt.Printf("Deleted file (Name: %s, URI: %s)\n", file.Name, file.URI) // Indicate successful deletion with Name and URI
		}
	}
}

func sentLlmPrompt(model *genai.GenerativeModel, prompt []genai.Part, ctx context.Context, file *os.File, batchNum int, client *genai.Client) { // Removed deleteChannel from parameters
	fmt.Printf("Sending batch %d to LLM...\n", batchNum)
	startTime := time.Now()
	resp, err := model.GenerateContent(ctx, prompt...)
	if err != nil {
		log.Println("Error generating content:", err)
		return
	}
	duration := time.Since(startTime)
	fmt.Printf("LLM response received in %v.\n", duration)
	for _, c := range resp.Candidates {
		if c.Content != nil {
			for _, part := range c.Content.Parts {
				if text, ok := part.(genai.Text); ok {
					if _, err := fmt.Fprintln(file, string(text)); err != nil {
						log.Println("Error writing to file:", err)
					}
				}
			}
		}
	}
	fmt.Printf("Batch %d processed and written to file.\n", batchNum)
	// Deletion is now handled in a separate goroutine after this function returns.
}

func promptMake(frame gocv.Mat, client *genai.Client, ctx context.Context, frameNo int) (*genai.File, error) {
	encode, err := gocv.IMEncode(".jpg", frame)
	if err != nil {
		return nil, fmt.Errorf("error encoding image: %w", err)
	}
	defer encode.Close()
	name := fmt.Sprintf("output_frame_%d.jpg", frameNo) // Generate unique filename based on frameNo
	if err := os.WriteFile(name, encode.GetBytes(), 0644); err != nil {
		fmt.Println("Error writing file:", err)
		return nil, err
	}

	file, err := client.UploadFileFromPath(ctx, name, nil) // Use unique filename for upload
	if err != nil {
		fmt.Println("Error Uploading file")
		return nil, err // Return error if upload fails
	}
	fmt.Printf("Uploaded file %s as: %s\n", name, file.URI)

	err = os.Remove(name) // Remove the unique named temporary file
	if err != nil {
		fmt.Printf("Error deleting temporary file : %v\n", err) // Clarify this is temp file deletion
	} else {
		fmt.Printf("Temporary file deleted successfully.\n") // Clarify this is temp file deletion
	}
	return file, nil
}

// getWorkerCounts determines the number of uploader and deletion workers based on system CPU cores and environment variables.
func getWorkerCounts() (numUploaders int, numDeletionWorkers int) {
	numCPU := runtime.NumCPU()
	fmt.Printf("System CPU cores: %d\n", numCPU)

	// --- Uploader Workers ---
	var baseUploaders int
	if envBaseUploaders := os.Getenv("BASE_UPLOADERS"); envBaseUploaders != "" {
		baseUploadersEnv, err := strconv.Atoi(envBaseUploaders)
		if err != nil {
			log.Printf("Warning: Invalid BASE_UPLOADERS environment variable '%s', using default.\n", envBaseUploaders)
			baseUploaders = 20 // Default if parsing fails
		} else {
			baseUploaders = baseUploadersEnv
		}
	} else {
		baseUploaders = 20 // Default
	}

	var uploadersPerCore int
	if envUploadersPerCore := os.Getenv("UPLOADERS_PER_CORE"); envUploadersPerCore != "" {
		uploadersPerCoreEnv, err := strconv.Atoi(envUploadersPerCore)
		if err != nil {
			log.Printf("Warning: Invalid UPLOADERS_PER_CORE environment variable '%s', using default.\n", envUploadersPerCore)
			uploadersPerCore = 10 // Default if parsing fails
		} else {
			uploadersPerCore = uploadersPerCoreEnv
		}
	} else {
		uploadersPerCore = 10 // Default
	}

	var maxUploaders int
	if envMaxUploaders := os.Getenv("MAX_UPLOADERS"); envMaxUploaders != "" {
		maxUploadersEnv, err := strconv.Atoi(envMaxUploaders)
		if err != nil {
			log.Printf("Warning: Invalid MAX_UPLOADERS environment variable '%s', using default.\n", envMaxUploaders)
			maxUploaders = 200 // Default if parsing fails
		} else {
			maxUploaders = maxUploadersEnv
		}
	} else {
		maxUploaders = 200 // Default
	}

	numUploadersCalculated := baseUploaders + (numCPU * uploadersPerCore)
	numUploaders = numUploadersCalculated
	if numUploaders > maxUploaders {
		numUploaders = maxUploaders
	}
	if numUploaders < 1 {
		numUploaders = 1
	}

	// --- Deletion Workers ---
	var deletionWorkersPerCore int
	if envDeletionWorkersPerCore := os.Getenv("DELETION_WORKERS_PER_CORE"); envDeletionWorkersPerCore != "" {
		deletionWorkersPerCoreEnv, err := strconv.Atoi(envDeletionWorkersPerCore)
		if err != nil {
			log.Printf("Warning: Invalid DELETION_WORKERS_PER_CORE environment variable '%s', using default.\n", envDeletionWorkersPerCore)
			deletionWorkersPerCore = 2 // Default if parsing fails
		} else {
			deletionWorkersPerCore = deletionWorkersPerCoreEnv
		}
	} else {
		deletionWorkersPerCore = 2 // Default
	}

	var minDeletionWorkers int
	if envMinDeletionWorkers := os.Getenv("MIN_DELETION_WORKERS"); envMinDeletionWorkers != "" {
		minDeletionWorkersEnv, err := strconv.Atoi(envMinDeletionWorkers)
		if err != nil {
			log.Printf("Warning: Invalid MIN_DELETION_WORKERS environment variable '%s', using default.\n", envMinDeletionWorkers)
			minDeletionWorkers = 2 // Default if parsing fails
		} else {
			minDeletionWorkers = minDeletionWorkersEnv
		}
	} else {
		minDeletionWorkers = 2 // Default
	}
	var maxDeletionWorkers int
	if envMaxDeletionWorkers := os.Getenv("MAX_DELETION_WORKERS"); envMaxDeletionWorkers != "" {
		maxDeletionWorkersEnv, err := strconv.Atoi(envMaxDeletionWorkers)
		if err != nil {
			log.Printf("Warning: Invalid MAX_DELETION_WORKERS environment variable '%s', using default.\n", envMaxDeletionWorkers)
			maxDeletionWorkers = 10 // Default if parsing fails
		} else {
			maxDeletionWorkers = maxDeletionWorkersEnv
		}
	} else {
		maxDeletionWorkers = 10 // Default
	}

	numDeletionWorkersCalculated := numCPU * deletionWorkersPerCore
	numDeletionWorkers = numDeletionWorkersCalculated
	if numDeletionWorkers < minDeletionWorkers {
		numDeletionWorkers = minDeletionWorkers
	}
	if numDeletionWorkers > maxDeletionWorkers {
		numDeletionWorkers = maxDeletionWorkers
	}

	// --- Explicit Override from Environment ---
	if envUploadWorkers := os.Getenv("UPLOAD_WORKERS"); envUploadWorkers != "" {
		uploadWorkersEnv, err := strconv.Atoi(envUploadWorkers)
		if err != nil {
			log.Printf("Warning: Invalid UPLOAD_WORKERS environment variable '%s', using calculated/default.\n", envUploadWorkers)
		} else {
			numUploaders = uploadWorkersEnv // Override calculated value
			fmt.Printf("UPLOAD_WORKERS environment variable set, overriding calculated uploaders to: %d\n", numUploaders)
		}
	} else {
		fmt.Printf("Calculated number of uploader workers: %d (based on CPU cores and defaults)\n", numUploaders)
	}

	if envDeletionWorkers := os.Getenv("DELETION_WORKERS"); envDeletionWorkers != "" {
		deletionWorkersEnv, err := strconv.Atoi(envDeletionWorkers)
		if err != nil {
			log.Printf("Warning: Invalid DELETION_WORKERS environment variable '%s', using calculated/default.\n", envDeletionWorkers)
		} else {
			numDeletionWorkers = deletionWorkersEnv // Override calculated value
			fmt.Printf("DELETION_WORKERS environment variable set, overriding calculated deletion workers to: %d\n", numDeletionWorkers)
		}
	} else {
		fmt.Printf("Calculated number of deletion workers: %d (based on CPU cores and defaults)\n", numDeletionWorkers)
	}

	return numUploaders, numDeletionWorkers
}

func main() {
	// Comment this out if you want to use audio transcription
	if len(os.Args) != 4 {
		fmt.Println("Usage: program <video_path> <llm_model> <api_key>")
		os.Exit(1)
	}
	path := os.Args[1]
	llm := os.Args[2]
	apiKey := os.Args[3]

	client, model, ctx := setLlmApi(llm, apiKey)
	defer func() {
		if err := client.Close(); err != nil {
			log.Println("Error closing genai client:", err)
		}
	}()

	fmt.Println("Opening video file...")
	video, err := gocv.VideoCaptureFile(path)
	if err != nil {
		log.Fatalf("Error opening video file: %v\n", err)
	}
	defer func() {
		if err := video.Close(); err != nil {
			log.Println("Error closing video:", err)
		}
	}()
	if !video.IsOpened() {
		log.Fatal("Cannot open video capture device")
	}
	fmt.Println("Video file opened successfully.")

	fmt.Println("Creating output file...")
	file, err := os.Create("output.txt")
	if err != nil {
		log.Fatalf("Error creating output file: %v\n", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Println("Error closing output file:", err)
		}
	}()
	fmt.Println("Output file created.")

	frameChannel := make(chan FrameData)         // Channel to send FrameData (frame and frame number)
	fileChannel := make(chan genai.File)         // Channel to send genai.File to main processing
	errorChannel := make(chan error, 10)         // Channel to collect errors from goroutines
	deleteChannel := make(chan genai.File, 1000) // Buffered channel for deletion requests - now expects genai.File
	var wg sync.WaitGroup                        // WaitGroup to wait for uploaders and deletion worker

	numUploaders, numDeletionWorkers := getWorkerCounts()

	// Start deletion workers
	for i := 0; i < numDeletionWorkers; i++ {
		wg.Add(1)
		go deletionWorker(client, ctx, deleteChannel, &wg)
	}

	// Frame Reading Goroutine
	go func() {
		defer close(frameChannel)
		defer fmt.Println("Frame reading complete.")
		frameNum := 0
		totalFrames := int(video.Get(gocv.VideoCaptureFrameCount))
		for {
			frame := gocv.NewMat()
			ok := video.Read(&frame)
			if !ok || frame.Empty() {
				frame.Close()
				break
			}
			frameNum++                                               // Increment frame number here, correctly sequencing frames
			frameData := FrameData{Frame: frame, FrameNum: frameNum} // Create FrameData struct
			frameChannel <- frameData                                // Send FrameData struct through the channel
			fmt.Printf("Reading frame %d of %d\r", frameNum, totalFrames)
		}
		fmt.Println()
	}()

	// Start uploader workers
	for i := 0; i < numUploaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("Uploader %d started.\n", i+1)
			defer fmt.Printf("Uploader %d finished.\n", i+1)
			for frameData := range frameChannel { // Receive FrameData struct
				frame := frameData.Frame                                   // Extract frame from FrameData
				frameNum := frameData.FrameNum                             // Extract frame number from FrameData
				imageData, err := promptMake(frame, client, ctx, frameNum) // Use the correct frameNum
				frame.Close()
				if err != nil {
					errorChannel <- fmt.Errorf("uploader error: %w", err)
					continue
				}
				if imageData != nil {
					fileChannel <- *imageData // Send genai.File to fileChannel
				}
			}
		}()
	}

	go func() {
		wg.Wait()            // Wait for all uploaders and deletion workers to finish
		close(fileChannel)   // Close fileChannel to signal no more files to process
		close(deleteChannel) // Close deleteChannel to signal deletion workers to stop
		fmt.Println("All uploaders and deletion workers finished.")
	}()

	var framelist []genai.File
	promptList := []genai.Part{genai.Text("## Task Description\nI'm providing multiple image frames containing a mix of handwritten and typed text, including mathematical notation. Please extract all text while preserving the original formatting and layout. This should include:\n\n1. All handwritten text\n2. All typed/printed text\n3. All mathematical equations, symbols, and notations\n4. The spatial relationships between different text elements\n5. Flowcharts, diagrams, and directional elements\n\n## Special Instructions\n\n### Mathematical Content\n- Preserve all mathematical symbols (×, ÷, ∑, ∫, √, etc.)\n- Maintain the structure of fractions, exponents, subscripts, and superscripts\n- Reproduce matrices and tables in their original grid format\n- Use proper notation for integrals, limits, and other calculus expressions\n- Ensure that equation numbering is preserved\n\n### Text Formatting\n- Maintain paragraph breaks and indentation\n- Preserve bullet points and numbered lists\n- Keep column and table structures intact\n- Reproduce text alignment (left, right, center)\n- Maintain special formatting like bold, italic, or underlined text where possible\n- Preserve footnotes and their references\n\n### Flowcharts and Diagrams\n- Capture the directional flow indicated by arrows (→, ↑, ↓, ←, ↔, etc.)\n- Preserve the relationships between connected elements in flowcharts\n- Maintain the sequence and hierarchy of flowchart components\n- Represent decision points and branches accurately\n- Capture labels on arrows and connecting lines\n- Preserve the overall structure of diagrams and their logical flow\n\n### Layout Preservation\n- Maintain the relative positioning of text blocks\n- Preserve the layout of multi-column text\n- Keep headers and footers distinct\n- Maintain page numbers and section markers\n- Preserve sidebars and margin notes in their correct positions\n\n## Frame Relationships\n- If content spans across multiple frames, indicate connections between them\n- Identify when a paragraph, equation, or other element continues to the next frame\n- Note any sequential numbering or logical flow across frames\n\n## Output Format\nPlease provide the extracted content in plain text format that mimics the original layout as closely as possible. For mathematical expressions, use standard ASCII/Unicode representations or LaTeX notation when necessary to preserve meaning. For flowcharts and diagrams, use appropriate ASCII art or text-based representations to show connections and flow.\n\n## Example\nIf Frame 1 contains:\n- A handwritten title at the top\n- Two columns of typed text\n- A mathematical equation in the middle\n- Handwritten margin notes\n- A simple flowchart with arrows connecting concepts\n\nYour response should:\n- Begin with the title\n- Present the two columns either side by side (if possible) or sequentially with clear markers\n- Reproduce the equation using appropriate notation\n- Include the margin notes with indicators of their original position\n- Represent the flowchart using text and symbols that preserve the directional relationships\n\n## Additional Notes\n- For difficult-to-read text, provide your best interpretation without marking uncertainty\n- If the orientation of text is unusual (vertical, diagonal, etc.), please note this\n- Some minor inaccuracies in text extraction are acceptable")}
	batchNum := 1

	// Main processing loop to assemble prompts and send to LLM
	for uploadedFile := range fileChannel { // Receive genai.File from fileChannel
		fileData := genai.FileData{URI: uploadedFile.URI} // Recreate FileData for promptList

		if len(promptList) <= 1000 {
			promptList = append(promptList, fileData)
			framelist = append(framelist, uploadedFile)

		} else {
			sentLlmPrompt(model, promptList, ctx, file, batchNum, client) // Send prompt to LLM

			// Delete files in a goroutine AFTER prompt is sent and response is received
			filesToDelete := make([]genai.File, len(framelist))
			copy(filesToDelete, framelist) // Create a copy to avoid race conditions
			go func(filesToDelete []genai.File, deleteChannel chan<- genai.File) {
				for _, fileToDelete := range filesToDelete {
					deleteChannel <- fileToDelete // Send genai.File to deleteChannel for deletion
				}
			}(filesToDelete, deleteChannel)

			promptList = []genai.Part{genai.Text("## Task Description\nI'm providing multiple image frames containing a mix of handwritten and typed text, including mathematical notation. Please extract all text while preserving the original formatting and layout. This should include:\n\n1. All handwritten text\n2. All typed/printed text\n3. All mathematical equations, symbols, and notations\n4. The spatial relationships between different text elements\n5. Flowcharts, diagrams, and directional elements\n\n## Special Instructions\n\n### Mathematical Content\n- Preserve all mathematical symbols (×, ÷, ∑, ∫, √, etc.)\n- Maintain the structure of fractions, exponents, subscripts, and superscripts\n- Reproduce matrices and tables in their original grid format\n- Use proper notation for integrals, limits, and other calculus expressions\n- Ensure that equation numbering is preserved\n\n### Text Formatting\n- Maintain paragraph breaks and indentation\n- Preserve bullet points and numbered lists\n- Keep column and table structures intact\n- Reproduce text alignment (left, right, center)\n- Maintain special formatting like bold, italic, or underlined text where possible\n- Preserve footnotes and their references\n\n### Flowcharts and Diagrams\n- Capture the directional flow indicated by arrows (→, ↑, ↓, ←, ↔, etc.)\n- Preserve the relationships between connected elements in flowcharts\n- Maintain the sequence and hierarchy of flowchart components\n- Represent decision points and branches accurately\n- Capture labels on arrows and connecting lines\n- Preserve the overall structure of diagrams and their logical flow\n\n### Layout Preservation\n- Maintain the relative positioning of text blocks\n- Preserve the layout of multi-column text\n- Keep headers and footers distinct\n- Maintain page numbers and section markers\n- Preserve sidebars and margin notes in their correct positions\n\n## Frame Relationships\n- If content spans across multiple frames, indicate connections between them\n- Identify when a paragraph, equation, or other element continues to the next frame\n- Note any sequential numbering or logical flow across frames\n\n## Output Format\nPlease provide the extracted content in plain text format that mimics the original layout as closely as possible. For mathematical expressions, use standard ASCII/Unicode representations or LaTeX notation when necessary to preserve meaning. For flowcharts and diagrams, use appropriate ASCII art or text-based representations to show connections and flow.\n\n## Example\nIf Frame 1 contains:\n- A handwritten title at the top\n- Two columns of typed text\n- A mathematical equation in the middle\n- Handwritten margin notes\n- A simple flowchart with arrows connecting concepts\n\nYour response should:\n- Begin with the title\n- Present the two columns either side by side (if possible) or sequentially with clear markers\n- Reproduce the equation using appropriate notation\n- Include the margin notes with indicators of their original position\n- Represent the flowchart using text and symbols that preserve the directional relationships\n\n## Additional Notes\n- For difficult-to-read text, provide your best interpretation without marking uncertainty\n- If the orientation of text is unusual (vertical, diagonal, etc.), please note this\n- Some minor inaccuracies in text extraction are acceptable"), fileData} // Start new batch with current fileData
			framelist = []genai.File{uploadedFile}                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             // start new framelist batch
			batchNum++
		}
	}

	// Process remaining frames in promptList if any
	if len(promptList) > 1 {
		sentLlmPrompt(model, promptList, ctx, file, batchNum, client) // Send prompt to LLM for last batch

		// Delete remaining files AFTER prompt is sent and response is received
		filesToDelete := make([]genai.File, len(framelist))
		copy(filesToDelete, framelist) // Create a copy to avoid race conditions
		go func(filesToDelete []genai.File, deleteChannel chan<- genai.File) {
			for _, fileToDelete := range filesToDelete {
				deleteChannel <- fileToDelete
			}
		}(filesToDelete, deleteChannel)
	}

	wg.Wait() // Wait for all uploaders and deletion worker to finish

	// Check for errors from goroutines
	close(errorChannel)
	for err := range errorChannel {
		log.Println("Error from goroutine:", err)
	}

	fmt.Println("\nProcessing complete.")
	fmt.Println("Exiting.")
	fmt.Println(framelist)
}
*/
/*
package main

import (

	"context"
	"fmt"
	"github.com/google/generative-ai-go/genai"
	"gocv.io/x/gocv"
	"google.golang.org/api/option"
	"log"
	"os"
	"time"

)

	func setLlmApi(llm string, apiKey string) (*genai.Client, *genai.GenerativeModel, context.Context) {
		ctx := context.Background()
		client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
		if err != nil {
			log.Fatal(err)
		}
		model := client.GenerativeModel(llm)
		fmt.Println("LLM API setup complete.") // Progress indicator
		return client, model, ctx
	}

	func sentLlmPrompt(model *genai.GenerativeModel, prompt []genai.Part, ctx context.Context, file *os.File, batchNum int, client genai.Client) {
		fmt.Printf("Sending batch %d to LLM...\n", batchNum) // Progress indicator with batch number
		startTime := time.Now()                              // Time the LLM call
		resp, err := model.GenerateContent(ctx, prompt...)
		if err != nil {
			log.Println("Error generating content:", err) // Log the error, don't panic
			return
		}

		duration := time.Since(startTime) // Calculate duration
		fmt.Printf("LLM response received in %v.\n", duration)
		for _, c := range resp.Candidates {
			if c.Content != nil {
				for _, part := range c.Content.Parts {
					if text, ok := part.(genai.Text); ok {
						if _, err := fmt.Fprintln(file, string(text)); err != nil {
							log.Println("Error writing to file:", err)
						}
					}
				}
			}
		}
		fmt.Printf("Batch %d processed and written to file.\n", batchNum)
		for _, part := range prompt {
			err := client.DeleteFile(ctx, part.(genai.FileData).URI)
			if err != nil {
				return
			}
		} // Progress indicator
	}

	func promptMake(frame gocv.Mat, client genai.Client, ctx context.Context) (*genai.File, error) {
		encode, err := gocv.IMEncode(".jpg", frame)
		if err != nil {
			return nil, fmt.Errorf("error encoding image: %w", err)
		}
		defer encode.Close()

		if err := os.WriteFile("output.jpg", encode.GetBytes(), 0644); err != nil {
			fmt.Println("Error writing file:", err)
			return nil, err
		}

		file, err := client.UploadFileFromPath(ctx, "output.jpg", nil)
		if err != nil {
			fmt.Println("Error Uploading file")
		}
		fmt.Printf("Uploaded file %s as: %w",
			"output.jpg", file.URI)
		err = os.Remove("output.jpg")
		if err != nil {
			fmt.Printf("Error deleting file : %v\n", err)
			// Consider whether to continue or return here.  If one delete fails,
			// do you want to try to delete the others?  Up to you.
		} else {
			fmt.Printf("File deleted successfully.\n")
		}
		return file, nil
	}

	func main() {
		if len(os.Args) != 4 {
			fmt.Println("Usage: program <video_path> <llm_model> <api_key>")
			os.Exit(1)
		}
		path := os.Args[1]
		llm := os.Args[2]
		apiKey := os.Args[3]

		client, model, ctx := setLlmApi(llm, apiKey)
		defer func() { // Close client *here*, at the end of main
			if err := client.Close(); err != nil {
				log.Println("Error closing genai client:", err)
			}
		}()

		fmt.Println("Opening video file...") // Progress indicator
		video, err := gocv.VideoCaptureFile(path)
		if err != nil {
			log.Fatalf("Error opening video file: %v\n", err)
		}
		defer func() {
			if err := video.Close(); err != nil {
				log.Println("Error closing video:", err)
			}
		}()
		if !video.IsOpened() {
			log.Fatal("Cannot open video capture device")
		}
		fmt.Println("Video file opened successfully.")

		fmt.Println("Creating output file...")
		file, err := os.Create("output.txt")
		if err != nil {
			log.Fatalf("Error creating output file: %v\n", err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				log.Println("Error closing output file:", err)
			}
		}()
		fmt.Println("Output file created.")

		var framesList []genai.Part
		framesList = append(framesList, genai.Text("start"))

		batchNum := 1                                              // Keep track of batch number
		frameNum := 0                                              //keep track of frame
		totalFrames := int(video.Get(gocv.VideoCaptureFrameCount)) //get total frames

		for {
			frame := gocv.NewMat()    // Create Mat inside the loop
			ok := video.Read(&frame)  // Pass pointer to Read
			if !ok || frame.Empty() { // Check for read error *and* empty frame
				err := frame.Close()
				if err != nil {
					log.Println("Error closing frame:", err)
				} //close the frame
				break // Exit loop on end of video or error
			}
			imageData, err := promptMake(frame, *client, ctx)
			frame.Close()
			if err != nil {
				log.Println("Error creating prompt:", err)
				continue
			}
			if len(framesList) <= 1000 {
				framesList = append(framesList, genai.FileData{URI: imageData.URI})

			} else if len(framesList) > 1000 {
				sentLlmPrompt(model, framesList, ctx, file, batchNum, *client)
				framesList = append(framesList, genai.FileData{URI: imageData.URI})
			}
			frameNum++
			fmt.Printf("Processing frame %d of %d\r", frameNum, totalFrames)

		}
		if len(framesList) > 1 {
			sentLlmPrompt(model, framesList, ctx, file, batchNum, *client)
		}
		fmt.Println("\nProcessing complete.") // Final progress indicator
		fmt.Println("Exiting.")
	}
*/
