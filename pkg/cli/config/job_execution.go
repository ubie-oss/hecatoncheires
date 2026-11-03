package config

import (
	"slices"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/pelletier/go-toml/v2"
)

// JobExecutionSection is scoped to one [[job]], never the deployment.
type JobExecutionSection struct {
	Cascade bool                      `toml:"cascade"`
	Mode    string                    `toml:"mode"`
	ApplyTo []string                  `toml:"apply_to"`
	Models  JobExecutionModelsSection `toml:"models"`
}

// JobExecutionModelsSection assigns reference names to execution roles.
type JobExecutionModelsSection struct {
	Main    string `toml:"main"`
	Sub     string `toml:"sub"`
	Advisor string `toml:"advisor"`
}

func (s *JobExecutionSection) toModel(legacyMain string) (*model.JobExecution, error) {
	if s == nil {
		return nil, nil
	}
	mode := model.JobExecutionMode(s.Mode)
	if mode == "" {
		mode = model.JobExecutionModeStandard
	}
	main := s.Models.Main
	if main == "" {
		main = legacyMain
	} else if legacyMain != "" && legacyMain != main {
		return nil, goerr.New("job llm_model and execution.models.main must agree")
	}
	sub := s.Models.Sub
	if sub == "" {
		sub = main
	}
	for _, role := range []struct{ name, ref string }{{"main", main}, {"sub", sub}, {"advisor", s.Models.Advisor}} {
		if role.ref != "" && (!llmModelRefPattern.MatchString(role.ref) || len(role.ref) > maxLLMModelRefLength) {
			return nil, goerr.Wrap(ErrInvalidLLMModelRef, "job execution model must be a model reference name", goerr.V("role", role.name), goerr.V("model_ref", role.ref))
		}
	}
	targets := []model.JobExecutionTarget{model.JobExecutionTargetJob, model.JobExecutionTargetJobSimple}
	if s.ApplyTo != nil {
		targets = make([]model.JobExecutionTarget, len(s.ApplyTo))
		for i, target := range s.ApplyTo {
			targets[i] = model.JobExecutionTarget(target)
		}
	}
	execution := &model.JobExecution{Mode: mode, Cascade: s.Cascade, ApplyTo: targets, Models: model.JobExecutionModels{Main: main, Sub: sub, Advisor: s.Models.Advisor}}
	if err := execution.Validate(); err != nil {
		return nil, err
	}
	return execution, nil
}

// ValidateJobExecutionTOML rejects unknown keys only inside job.execution.
// Existing workspace keys retain their historical permissive TOML behavior.
// Config loaders call this before decoding the typed document.
func ValidateJobExecutionTOML(data []byte) error {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return err
	}
	jobs, _ := raw["job"].([]any)
	for i, value := range jobs {
		job, ok := value.(map[string]any)
		if !ok {
			continue // The typed decoder reports malformed [[job]] shapes.
		}
		value, exists := job["execution"]
		if !exists {
			continue
		}
		execution, ok := value.(map[string]any)
		if !ok {
			return goerr.New("job.execution must be a table", goerr.V("job_index", i))
		}
		for key, value := range execution {
			if !slices.Contains([]string{"mode", "cascade", "apply_to", "models"}, key) {
				return goerr.New("unknown job.execution key", goerr.V("key", key), goerr.V("job_index", i))
			}
			switch key {
			case "mode":
				if _, ok := value.(string); !ok {
					return goerr.New("job.execution.mode must be a string")
				}
			case "cascade":
				if _, ok := value.(bool); !ok {
					return goerr.New("job.execution.cascade must be a boolean")
				}
			case "apply_to":
				targets, ok := value.([]any)
				if !ok {
					return goerr.New("job.execution.apply_to must be an array of strings")
				}
				for _, target := range targets {
					if _, ok := target.(string); !ok {
						return goerr.New("job.execution.apply_to must be an array of strings")
					}
				}
			}
			if key != "models" {
				continue
			}
			models, ok := value.(map[string]any)
			if !ok {
				return goerr.New("job.execution.models must be a table", goerr.V("job_index", i))
			}
			for role, ref := range models {
				if _, ok := ref.(string); !ok {
					return goerr.New("job.execution model roles must be strings", goerr.V("role", role))
				}
				if !slices.Contains([]string{"main", "sub", "advisor"}, role) {
					return goerr.New("unknown job.execution.models key", goerr.V("key", role), goerr.V("job_index", i))
				}
			}
		}
	}
	return nil
}
