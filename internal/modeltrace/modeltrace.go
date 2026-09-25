package modeltrace

import (
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

const modelTraceFallbackCommit = "55a2e4a55170423b484d701e9a82ab62b268c811"
const modelTraceDimension = 355

//go:embed modeltrace_unified_bank.json
var embeddedModelTraceBank []byte

var modelTraceNumbers = regexp.MustCompile(`[0-9]+`)

type modelTraceModel struct {
	ID     string `json:"id"`
	Family string `json:"family"`
}

type modelTraceArtifact struct {
	Weight               float64       `json:"weight"`
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
}

type modelTraceBank struct {
	Schema string            `json:"schema"`
	Models []modelTraceModel `json:"models"`
	Robust struct {
		ModelOrder    []string           `json:"model_order"`
		Hellinger     modelTraceArtifact `json:"hellinger"`
		OrderedBlocks modelTraceArtifact `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]struct {
		Beta float64 `json:"beta"`
	} `json:"calibration"`
}

type ModelTracePrediction struct {
	Model       string
	Probability float64
	ParsedCount int
}

type modelTraceVersion struct {
	bank   *modelTraceBank
	commit string
}

var modelTraceActive atomic.Pointer[modelTraceVersion]

var modelTraceFallback = sync.OnceValues(func() (*modelTraceBank, error) {
	return parseModelTraceBank(embeddedModelTraceBank)
})

func parseModelTraceBank(data []byte) (*modelTraceBank, error) {
	var bank modelTraceBank
	if err := json.Unmarshal(data, &bank); err != nil {
		return nil, err
	}
	if bank.Schema != "robust-number-fingerprint-bank" || len(bank.Models) == 0 || len(bank.Models) != len(bank.Robust.ModelOrder) {
		return nil, errors.New("invalid ModelTrace bank schema")
	}
	for index, model := range bank.Models {
		if model.ID == "" || model.ID != bank.Robust.ModelOrder[index] {
			return nil, errors.New("invalid ModelTrace model order")
		}
	}
	for _, artifact := range []struct {
		data      modelTraceArtifact
		dimension int
	}{
		{bank.Robust.Hellinger, modelTraceDimension},
		{bank.Robust.OrderedBlocks, 74},
	} {
		if len(artifact.data.FeatureMean) != artifact.dimension || len(artifact.data.FeatureScale) != artifact.dimension || len(artifact.data.Centroids) != len(bank.Models) {
			return nil, errors.New("invalid ModelTrace feature dimensions")
		}
		for _, scale := range artifact.data.FeatureScale {
			if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
				return nil, errors.New("invalid ModelTrace feature scale")
			}
		}
		for _, centroid := range artifact.data.Centroids {
			if len(centroid) != artifact.dimension {
				return nil, errors.New("invalid ModelTrace centroid")
			}
		}
		for _, basis := range artifact.data.NuisanceBasis {
			if len(basis) != artifact.dimension {
				return nil, errors.New("invalid ModelTrace nuisance basis")
			}
		}
	}
	for _, environment := range bank.Robust.OrderedBlocks.EnvironmentCentroids {
		if len(environment) != len(bank.Models) {
			return nil, errors.New("invalid ModelTrace environment")
		}
		for _, centroid := range environment {
			if len(centroid) != 74 {
				return nil, errors.New("invalid ModelTrace environment centroid")
			}
		}
	}
	if len(bank.Robust.OrderedBlocks.EnvironmentCentroids) == 0 || bank.Calibration["1"].Beta <= 0 {
		return nil, errors.New("missing ModelTrace calibration")
	}
	return &bank, nil
}

func ModelTraceGPTModels() ([]string, error) {
	snapshot, err := currentModelTraceBank()
	if err != nil {
		return nil, err
	}
	bank := snapshot.bank
	models := make([]string, 0, len(bank.Models))
	for _, model := range bank.Models {
		if model.Family == "gpt" {
			models = append(models, model.ID)
		}
	}
	return models, nil
}

func modelTraceKnownGPTModel(model string) bool {
	models, err := ModelTraceGPTModels()
	if err != nil {
		return false
	}
	for _, known := range models {
		if known == model {
			return true
		}
	}
	return false
}

func codexTicketEligibleModel(model string) bool {
	version := strings.TrimPrefix(model, "gpt-")
	if version == model {
		return false
	}
	majorText, remainder, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(strings.SplitN(majorText, "-", 2)[0])
	if err != nil {
		return false
	}
	if major != 5 {
		return major > 5
	}
	minor, err := strconv.Atoi(strings.SplitN(remainder, "-", 2)[0])
	return err == nil && minor >= 6
}

func ModelTraceTicketModels() ([]string, error) {
	models, err := ModelTraceGPTModels()
	if err != nil {
		return nil, err
	}
	eligible := make([]string, 0, len(models))
	for _, model := range models {
		if codexTicketEligibleModel(model) {
			eligible = append(eligible, model)
		}
	}
	return eligible, nil
}

func modelTraceParseNumbers(text string) []int {
	matches := modelTraceNumbers.FindAllStringIndex(text, -1)
	var longest, current []int
	previousEnd := 0
	for _, match := range matches {
		separator := text[previousEnd:match[0]]
		if len(current) > 0 && strings.IndexFunc(separator, unicode.IsLetter) >= 0 {
			if len(current) > len(longest) {
				longest = current
			}
			current = nil
		}
		value, err := strconv.Atoi(text[match[0]:match[1]])
		if err == nil && value >= 1 && value <= modelTraceDimension {
			current = append(current, value)
		}
		previousEnd = match[1]
	}
	if len(current) > len(longest) {
		return current
	}
	return longest
}

func modelTraceDot(left, right []float64) float64 {
	result := 0.0
	for index, value := range left {
		result += value * right[index]
	}
	return result
}

func modelTraceNormalize(values []float64) []float64 {
	length := math.Max(math.Sqrt(modelTraceDot(values, values)), 1e-12)
	output := make([]float64, len(values))
	for index, value := range values {
		output[index] = value / length
	}
	return output
}

func modelTraceStandardize(values []float64) []float64 {
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, value := range values {
		variance += (value - mean) * (value - mean)
	}
	scale := math.Max(math.Sqrt(variance/float64(len(values))), 1e-12)
	result := make([]float64, len(values))
	for index, value := range values {
		result[index] = (value - mean) / scale
	}
	return result
}

func modelTraceProject(feature []float64, artifact modelTraceArtifact) []float64 {
	projected := make([]float64, len(feature))
	for index, value := range feature {
		projected[index] = (value - artifact.FeatureMean[index]) / artifact.FeatureScale[index]
	}
	return projected
}

func modelTraceSubtractBasis(values []float64, basis [][]float64) []float64 {
	result := append([]float64(nil), values...)
	for _, vector := range basis {
		projection := modelTraceDot(result, vector)
		for index := range result {
			result[index] -= projection * vector[index]
		}
	}
	return result
}

func modelTraceCentroidScores(feature []float64, centroids [][]float64) []float64 {
	scores := make([]float64, len(centroids))
	for index, centroid := range centroids {
		scores[index] = modelTraceDot(feature, centroid)
	}
	return scores
}

func modelTraceScores(numbers []int, bank *modelTraceBank) []float64 {
	counts := make([]float64, modelTraceDimension)
	for _, number := range numbers {
		counts[number-1]++
	}
	hellinger := make([]float64, modelTraceDimension)
	total := float64(len(numbers)) + 0.5*modelTraceDimension
	for index, count := range counts {
		hellinger[index] = math.Sqrt((count + 0.5) / total)
	}
	marginalArtifact := bank.Robust.Hellinger
	projected := modelTraceNormalize(modelTraceSubtractBasis(modelTraceProject(hellinger, marginalArtifact), marginalArtifact.NuisanceBasis))
	marginal := modelTraceStandardize(modelTraceCentroidScores(projected, marginalArtifact.Centroids))
	orderedArtifact := bank.Robust.OrderedBlocks
	if orderedArtifact.Weight == 0 {
		return marginal
	}
	feature := make([]float64, 0, 74)
	base, remainder, start := len(numbers)/4, len(numbers)%4, 0
	for block := 0; block < 4; block++ {
		size := base
		if block < remainder {
			size++
		}
		bins := make([]float64, 16)
		for index := range bins {
			bins[index] = 0.5
		}
		for _, value := range numbers[start : start+size] {
			index := int(math.Floor(float64(value-1) / 355 * 16))
			if index > 15 {
				index = 15
			}
			bins[index]++
		}
		denominator := float64(size) + 8
		for _, value := range bins {
			feature = append(feature, math.Sqrt(value/denominator))
		}
		start += size
	}
	lastDigits := make([]float64, 10)
	for index := range lastDigits {
		lastDigits[index] = 0.5
	}
	for _, number := range numbers {
		lastDigits[number%10]++
	}
	for _, value := range lastDigits {
		feature = append(feature, math.Sqrt(value/(float64(len(numbers))+5)))
	}
	standardized := modelTraceProject(feature, orderedArtifact)
	unit := modelTraceNormalize(standardized)
	templateRaw := make([]float64, len(bank.Models))
	for modelIndex := range templateRaw {
		templateRaw[modelIndex] = math.Inf(-1)
		for _, environment := range orderedArtifact.EnvironmentCentroids {
			templateRaw[modelIndex] = math.Max(templateRaw[modelIndex], modelTraceDot(unit, environment[modelIndex]))
		}
	}
	template := modelTraceStandardize(templateRaw)
	nuisanceFeature := modelTraceNormalize(modelTraceSubtractBasis(standardized, orderedArtifact.NuisanceBasis))
	nuisance := modelTraceStandardize(modelTraceCentroidScores(nuisanceFeature, orderedArtifact.Centroids))
	fused := make([]float64, len(bank.Models))
	for index := range fused {
		fused[index] = 0.5*template[index] + 0.5*nuisance[index]
	}
	ordered := modelTraceStandardize(fused)
	for index := range marginal {
		marginal[index] = (1-orderedArtifact.Weight)*marginal[index] + orderedArtifact.Weight*ordered[index]
	}
	return marginal
}

func ModelTracePredict(text string, expectedCount int) (ModelTracePrediction, error) {
	prediction, _, err := ModelTracePredictCommitted(text, expectedCount)
	return prediction, err
}

func ModelTracePredictCommitted(text string, expectedCount int) (ModelTracePrediction, string, error) {
	snapshot, err := currentModelTraceBank()
	if err != nil {
		return ModelTracePrediction{}, "", err
	}
	bank := snapshot.bank
	numbers := modelTraceParseNumbers(text)
	minimum := 80
	if expectedCount > 0 {
		minimum = max(minimum, int(math.Ceil(float64(expectedCount)*0.55)))
	}
	if len(numbers) < minimum {
		return ModelTracePrediction{ParsedCount: len(numbers)}, snapshot.commit, errors.New("insufficient_numbers")
	}
	scores := modelTraceScores(numbers, bank)
	beta := bank.Calibration["1"].Beta
	maximum := math.Inf(-1)
	for _, value := range scores {
		maximum = math.Max(maximum, beta*value)
	}
	weights := make([]float64, len(scores))
	total := 0.0
	winner := 0
	for index, score := range scores {
		weights[index] = math.Exp(beta*score - maximum)
		total += weights[index]
		if score > scores[winner] {
			winner = index
		}
	}
	return ModelTracePrediction{Model: bank.Models[winner].ID, Probability: weights[winner] / total, ParsedCount: len(numbers)}, snapshot.commit, nil
}

func currentModelTraceBank() (*modelTraceVersion, error) {
	if active := modelTraceActive.Load(); active != nil {
		return active, nil
	}
	bank, err := modelTraceFallback()
	if err != nil {
		return nil, err
	}
	return &modelTraceVersion{bank: bank, commit: modelTraceFallbackCommit}, nil
}

func ModelTraceBankCommit() string {
	snapshot, err := currentModelTraceBank()
	if err != nil {
		return ""
	}
	return snapshot.commit
}
