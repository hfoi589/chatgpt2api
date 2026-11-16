package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
)

const maxActiveTasks = 10

var ErrTooManyTasks = errors.New("进行中的任务太多了，请等一部分完成后再提交")

type TaskService struct {
	repo     *repository.Repository
	ai       *AIService
	quotas   *QuotaService
	config   *config.Config
}

func NewTaskService(repo *repository.Repository, ai *AIService, quotas *QuotaService, config *config.Config) *TaskService {
	return &TaskService{repo: repo, ai: ai, quotas: quotas, config: config}
}

func (s *TaskService) List(ctx context.Context, ownerID string, ids []int64, types []string) ([]repository.Task, []int64, error) {
	if err := s.CleanupExpired(ctx); err != nil {
		return nil, nil, err
	}
	items, err := s.repo.Tasks(ctx, ownerID, ids, types)
	if err != nil {
		return nil, nil, err
	}
	found := map[int64]bool{}
	for _, item := range items {
		found[item.ID] = true
	}
	missing := []int64{}
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return items, missing, nil
}

func (s *TaskService) SubmitImage(ctx context.Context, identity Identity, request ImageRequest, historyID *int64, turnID *int64, kind string) (repository.Task, error) {
	if request.Model == "" || request.Model == "auto" {
		request.Model = s.config.ImageModelName
	}
	active, err := s.repo.CountActiveTasks(ctx, identity.ID)
	if err != nil {
		return repository.Task{}, err
	}
	if active >= maxActiveTasks {
		return repository.Task{}, ErrTooManyTasks
	}
	if s.config.BaseURL != "" {
		request.BaseURL = s.config.BaseURL
	}
	reservation, err := s.quotas.Reserve(ctx, identity, request.Count)
	if err != nil {
		return repository.Task{}, &ImageError{Err: err, Status: 429, Type: "insufficient_quota", Code: "insufficient_quota"}
	}
	taskTitle := title(request.Prompt)
	if historyID != nil {
		if history, err := s.repo.Task(ctx, identity.ID, *historyID); err == nil {
			taskTitle = history.Title
		}
	}
	now := time.Now().UTC()
	item := repository.Task{OwnerID: identity.ID, HistoryID: historyID, TurnID: turnID, Title: taskTitle, Type: kind, Status: "queued", Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality, OutputCount: request.Count, BaseURL: request.BaseURL, CreatedAt: now, UpdatedAt: now}
	if reservation != nil { item.QuotaReserved, item.QuotaKeyID = reservation.Amount, reservation.KeyID }
	item, err = s.repo.SaveTask(ctx, item)
	if err != nil {
		_ = s.quotas.Settle(context.Background(), reservation, 0)
		return item, err
	}
	if item.HistoryID == nil {
		item.HistoryID = &item.ID
	}
	if item.TurnID == nil {
		item.TurnID = &item.ID
	}
	item, err = s.repo.SaveTask(ctx, item)
	if err == nil {
		go s.runImage(identity, item, request, reservation)
	} else {
		_ = s.quotas.Settle(context.Background(), reservation, 0)
	}
	return item, err
}

func (s *TaskService) runImage(identity Identity, task repository.Task, request ImageRequest, reservation *QuotaReservation) {
	ctx := context.Background()
	now := time.Now().UTC()
	task.Status, task.Progress, task.StartedAt, task.UpdatedAt = "running", "", &now, now
	s.repo.SaveTask(ctx, task)
	started := time.Now()
	callLog := s.ai.logs.NewCallLog(identity, map[string]string{"image_generation": "/api/tasks/image-generation", "image_edit": "/api/tasks/image-edit"}[task.Type], task.Model, task.Type+" ", task.Prompt, started)
	results, err := s.ai.ImagesWithReservation(ctx, request, callLog, reservation)
	task.QuotaReserved, task.QuotaKeyID = 0, ""
	ended := time.Now().UTC()
	duration := int(time.Since(started).Milliseconds())
	task.EndedAt, task.UpdatedAt, task.DurationMS = &ended, ended, &duration
	if err != nil {
		var imageErr *ImageError
		if errors.As(err, &imageErr) {
			task.ConversationID = imageErr.ConversationID
			task.AccountEmail = imageErr.AccountEmail
		}
		task.Status, task.Error, task.Progress = "error", err.Error(), ""
		s.repo.SaveTask(ctx, task)
		return
	}
	output := make([]repository.TaskResult, 0, len(results))
	for index, result := range results {
		output = append(output, repository.TaskResult{TaskID: task.ID, Position: index, Type: "image", Name: "image-" + strconv.Itoa(index+1) + ".png", URL: result.URL, RevisedPrompt: result.RevisedPrompt, ContentType: "image/png"})
	}
	s.repo.ReplaceTaskResults(ctx, task.ID, output)
	task.InputTokens, task.OutputTokens, task.TotalTokens = imageTaskUsage(request, results)
	task.Status, task.Progress = "success", ""
	s.repo.SaveTask(ctx, task)
}

func (s *TaskService) RecoverInterrupted(ctx context.Context) error {
	items, err := s.repo.InterruptActiveTasks(ctx, time.Now().UTC())
	if err != nil { return err }
	for _, item := range items {
		if item.QuotaReserved > 0 && item.QuotaKeyID != "" {
			actual := len(item.Results)
			if actual > item.QuotaReserved { actual = item.QuotaReserved }
			if err := s.quotas.Settle(ctx, &QuotaReservation{KeyID: item.QuotaKeyID, Amount: item.QuotaReserved}, actual); err != nil { return err }
		}
	}
	return nil
}

func (s *TaskService) CleanupExpired(ctx context.Context) error {
	return s.repo.DeleteFinishedTasksBefore(ctx, time.Now().AddDate(0, 0, -s.config.Image.RetentionDays))
}
func title(value string) string {
	runes := []rune(value)
	if len(runes) > 24 {
		return string(runes[:24])
	}
	return value
}
