package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/pipeline"
)

// PipelineStore persists ingest pipelines.
type PipelineStore interface {
	ListPipelines(ctx context.Context) ([]pipeline.Spec, error)
	GetPipeline(ctx context.Context, id string) (pipeline.Spec, error)
	PutPipeline(ctx context.Context, s pipeline.Spec) (pipeline.Spec, error)
	DeletePipeline(ctx context.Context, id string) error
}

// reloadPipelines re-reads every pipeline and installs the compiled set, so an
// edit takes effect on the next event rather than the next restart.
func (s *Server) reloadPipelines(ctx context.Context) error {
	if s.pipelines == nil || s.pipelineSet == nil {
		return nil
	}
	specs, err := s.pipelines.ListPipelines(ctx)
	if err != nil {
		return err
	}
	return s.pipelineSet.Replace(specs)
}

func (s *Server) handlePipelinesList(w http.ResponseWriter, r *http.Request) {
	specs, err := s.pipelines.ListPipelines(r.Context())
	if err != nil {
		s.logger.Error("list pipelines failed", "error", err)
		http.Error(w, "could not list pipelines", http.StatusInternalServerError)
		return
	}
	if specs == nil {
		specs = []pipeline.Spec{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pipelines": specs,
		"patterns":  pipeline.PatternNames(),
	})
}

func (s *Server) handlePipelineCreate(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeJSON[pipeline.Spec](w, r)
	if !ok {
		return
	}
	spec.ID = ""
	s.savePipeline(w, r, spec, http.StatusCreated)
}

func (s *Server) handlePipelineUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.pipelines.GetPipeline(r.Context(), id); err != nil {
		s.pipelineNotFound(w, err)
		return
	}
	spec, ok := decodeJSON[pipeline.Spec](w, r)
	if !ok {
		return
	}
	spec.ID = id
	s.savePipeline(w, r, spec, http.StatusOK)
}

// savePipeline compiles before persisting, so a bad pattern is a 400 at write
// time rather than a stage that silently fails on every event afterwards.
func (s *Server) savePipeline(w http.ResponseWriter, r *http.Request, spec pipeline.Spec, okStatus int) {
	if _, err := pipeline.Compile(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.pipelines.PutPipeline(r.Context(), spec)
	if err != nil {
		s.logger.Error("save pipeline failed", "error", err)
		http.Error(w, "could not save the pipeline", http.StatusInternalServerError)
		return
	}
	if rerr := s.reloadPipelines(r.Context()); rerr != nil {
		// The write succeeded, so report that honestly; the set will pick the
		// change up on the next successful reload.
		s.logger.Error("pipeline saved but the active set could not be reloaded", "error", rerr)
	}
	s.logger.Info("pipeline saved", "pipeline", saved.Name, "id", saved.ID, "enabled", saved.Enabled)
	writeJSON(w, okStatus, saved)
}

func (s *Server) handlePipelineDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.pipelines.DeletePipeline(r.Context(), r.PathValue("id")); err != nil {
		s.pipelineNotFound(w, err)
		return
	}
	if rerr := s.reloadPipelines(r.Context()); rerr != nil {
		s.logger.Error("pipeline deleted but the active set could not be reloaded", "error", rerr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// pipelineTestRequest is a dry run: a sample line plus the pipelines to try.
// Grok is unforgiving enough that writing a pattern without being able to test
// it against a real line is guesswork.
type pipelineTestRequest struct {
	Line      string            `json:"line"`
	Service   string            `json:"service,omitempty"`
	Source    string            `json:"source,omitempty"`
	Pipelines []pipeline.Spec   `json:"pipelines,omitempty"` // omitted = use the saved ones
	Fields    map[string]string `json:"fields,omitempty"`    // seed attributes
}

func (s *Server) handlePipelineTest(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[pipelineTestRequest](w, r)
	if !ok {
		return
	}
	specs := req.Pipelines
	if len(specs) == 0 {
		saved, err := s.pipelines.ListPipelines(r.Context())
		if err != nil {
			http.Error(w, "could not load pipelines", http.StatusInternalServerError)
			return
		}
		specs = saved
	}

	e := model.LogEvent{Message: req.Line, Raw: req.Line, Service: req.Service, Source: req.Source}
	if len(req.Fields) > 0 {
		e.Attributes = map[string]any{}
		for k, v := range req.Fields {
			e.Attributes[k] = v
		}
	}
	e.Normalize(s.now())

	res, err := pipeline.Test(specs, e)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) pipelineNotFound(w http.ResponseWriter, err error) {
	if errors.Is(err, pipeline.ErrNotFound) {
		http.Error(w, "pipeline not found", http.StatusNotFound)
		return
	}
	s.logger.Error("pipeline store operation failed", "error", err)
	http.Error(w, "pipeline store error", http.StatusInternalServerError)
}
