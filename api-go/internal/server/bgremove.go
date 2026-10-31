package server

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"
	ort "github.com/yalue/onnxruntime_go"
)

// Background removal runs the same ONNX models and pre/post-processing as rembg
// (https://github.com/danielgatis/rembg). The model produces an alpha mask, which is
// stored per asset and composited onto the original for every bg_remove variant.

type bgModel struct {
	file string
	size int
	mean [3]float32
	std  [3]float32
}

var bgModels = map[string]bgModel{
	// Apache-2.0
	"isnet": {file: "isnet-general-use.onnx", size: 1024, mean: [3]float32{0.5, 0.5, 0.5}, std: [3]float32{1, 1, 1}},
	// Apache-2.0
	"u2netp": {file: "u2netp.onnx", size: 320, mean: [3]float32{0.485, 0.456, 0.406}, std: [3]float32{0.229, 0.224, 0.225}},
}

var errBGDisabled = errors.New("background removal is disabled on this server")

type bgRemover struct {
	model     bgModel
	modelPath string
	libPath   string
	slots     chan struct{}

	once    sync.Once
	initErr error
	session *ort.DynamicAdvancedSession
}

func newBGRemover(cfg Config) *bgRemover {
	if cfg.BGModel == "off" {
		return nil
	}
	m := bgModels[cfg.BGModel]
	return &bgRemover{
		model:     m,
		modelPath: filepath.Join(cfg.BGModelDir, m.file),
		libPath:   cfg.OnnxRuntimeLib,
		slots:     make(chan struct{}, cfg.BGConcurrency),
	}
}

// init loads ONNX Runtime and the model on first use, so servers that never remove
// backgrounds don't pay for the memory.
func (b *bgRemover) init() error {
	b.once.Do(func() {
		ort.SetSharedLibraryPath(b.libPath)
		if !ort.IsInitialized() {
			if err := ort.InitializeEnvironment(); err != nil {
				b.initErr = fmt.Errorf("initialize onnxruntime: %w", err)
				return
			}
		}
		inputs, outputs, err := ort.GetInputOutputInfo(b.modelPath)
		if err != nil {
			b.initErr = fmt.Errorf("read model %s: %w", b.modelPath, err)
			return
		}
		if len(inputs) == 0 || len(outputs) == 0 {
			b.initErr = fmt.Errorf("model %s has no inputs or outputs", b.modelPath)
			return
		}
		b.session, err = ort.NewDynamicAdvancedSession(b.modelPath,
			[]string{inputs[0].Name}, []string{outputs[0].Name}, nil)
		if err != nil {
			b.initErr = fmt.Errorf("load model %s: %w", b.modelPath, err)
		}
	})
	return b.initErr
}

// mask computes an 8-bit alpha mask (PNG) the size of the auto-rotated original.
func (b *bgRemover) mask(original []byte) ([]byte, error) {
	if b == nil {
		return nil, errBGDisabled
	}
	if err := b.init(); err != nil {
		return nil, err
	}
	b.slots <- struct{}{}
	defer func() { <-b.slots }()

	header, err := vips.NewImageFromBuffer(original)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	srcW, srcH := displaySize(header)
	header.Close()

	size := b.model.size
	input, err := b.inputTensor(original, size)
	if err != nil {
		return nil, err
	}
	defer input.Destroy()
	output, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, int64(size), int64(size)))
	if err != nil {
		return nil, err
	}
	defer output.Destroy()
	if err := b.session.Run([]ort.Value{input}, []ort.Value{output}); err != nil {
		return nil, fmt.Errorf("run model: %w", err)
	}

	// Min-max normalize the prediction to 0..255.
	pred := output.GetData()
	lo, hi := float32(math.Inf(1)), float32(math.Inf(-1))
	for _, v := range pred {
		lo, hi = min(lo, v), max(hi, v)
	}
	span := hi - lo
	if span == 0 {
		span = 1
	}
	pixels := make([]byte, len(pred))
	for i, v := range pred {
		pixels[i] = byte((v - lo) / span * 255)
	}

	maskImg, err := vips.NewImageFromMemory(pixels, size, size, 1, vips.BandFormatUchar, vips.InterpretationBW)
	if err != nil {
		return nil, fmt.Errorf("mask image: %w", err)
	}
	defer maskImg.Close()
	if err := maskImg.ResizeWithVScale(float64(srcW)/float64(size), float64(srcH)/float64(size), vips.KernelLanczos3); err != nil {
		return nil, fmt.Errorf("resize mask: %w", err)
	}
	out, _, err := maskImg.ExportPng(&vips.PngExportParams{Compression: 6, StripMetadata: true})
	return out, err
}

// inputTensor resizes the image to size×size RGB and normalizes it like rembg:
// divide by the image maximum, subtract mean, divide by std, CHW layout.
func (b *bgRemover) inputTensor(original []byte, size int) (*ort.Tensor[float32], error) {
	img, err := vips.LoadThumbnailFromBuffer(original, size, size, vips.InterestingNone, vips.SizeForce, vips.NewImportParams())
	if err != nil {
		return nil, fmt.Errorf("resize for model: %w", err)
	}
	defer img.Close()
	if img.Interpretation() != vips.InterpretationSRGB {
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return nil, err
		}
	}
	if img.Bands() > 3 {
		if err := img.ExtractBand(0, 3); err != nil {
			return nil, err
		}
	}
	if img.BandFormat() != vips.BandFormatUchar {
		if err := img.Cast(vips.BandFormatUchar); err != nil {
			return nil, err
		}
	}
	raw, err := img.ToBytes()
	if err != nil {
		return nil, err
	}
	if len(raw) != size*size*3 {
		return nil, fmt.Errorf("unexpected pixel buffer size %d", len(raw))
	}

	var maxValue byte = 1
	for _, v := range raw {
		maxValue = max(maxValue, v)
	}
	plane := size * size
	data := make([]float32, 3*plane)
	for i := 0; i < plane; i++ {
		for c := 0; c < 3; c++ {
			v := float32(raw[i*3+c]) / float32(maxValue)
			data[c*plane+i] = (v - b.model.mean[c]) / b.model.std[c]
		}
	}
	return ort.NewTensor(ort.NewShape(1, 3, int64(size), int64(size)), data)
}

// cutout returns the auto-rotated original with the mask as its alpha channel, encoded
// as a fast PNG for the normal resize pipeline. With a background color it is flattened.
func cutout(original, maskPNG []byte, bgc string) ([]byte, error) {
	img, err := vips.LoadImageFromBuffer(original, vips.NewImportParams())
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	defer img.Close()
	if err := img.AutoRotate(); err != nil {
		return nil, err
	}
	if img.Interpretation() != vips.InterpretationSRGB {
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return nil, err
		}
	}
	if img.Bands() > 3 {
		if err := img.ExtractBand(0, 3); err != nil {
			return nil, err
		}
	}
	mask, err := vips.NewImageFromBuffer(maskPNG)
	if err != nil {
		return nil, fmt.Errorf("load mask: %w", err)
	}
	defer mask.Close()
	if mask.Width() != img.Width() || mask.Height() != img.Height() {
		return nil, fmt.Errorf("mask is %dx%d, image is %dx%d", mask.Width(), mask.Height(), img.Width(), img.Height())
	}
	if err := img.BandJoin(mask); err != nil {
		return nil, fmt.Errorf("apply mask: %w", err)
	}
	if bgc != "" {
		var r, g, b uint8
		fmt.Sscanf(bgc, "%02x%02x%02x", &r, &g, &b)
		if err := img.Flatten(&vips.Color{R: r, G: g, B: b}); err != nil {
			return nil, err
		}
	}
	out, _, err := img.ExportPng(&vips.PngExportParams{Compression: 1, StripMetadata: true})
	return out, err
}
