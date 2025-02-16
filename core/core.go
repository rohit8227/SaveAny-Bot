package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"github.com/krau/SaveAny-Bot/bot"
	"github.com/krau/SaveAny-Bot/config"
	"github.com/krau/SaveAny-Bot/logger"
	"github.com/krau/SaveAny-Bot/queue"
	"github.com/krau/SaveAny-Bot/types"
)

func processPendingTask(task *types.Task) error {
	logger.L.Debugf("Start processing task: %s", task.String())
	os.MkdirAll(config.Cfg.Temp.BasePath, os.ModePerm)

	ctx := task.Ctx.(*ext.Context)
	ctx.EditMessage(task.ChatID, &tg.MessagesEditMessageRequest{
		Message: "正在下载: " + task.String(),
		ID:      task.ReplyMessageID,
	})

	destPath := filepath.Join(config.Cfg.Temp.BasePath, task.File.FileName)
	if task.StoragePath == "" {
		task.StoragePath = task.File.FileName
	}

	// process photo
	if task.File.FileSize == 0 {
		res, err := bot.Client.API().UploadGetFile(task.Ctx, &tg.UploadGetFileRequest{
			Location: task.File.Location,
			Offset:   0,
			Limit:    1024 * 1024,
		})
		if err != nil {
			return fmt.Errorf("Failed to get file: %w", err)
		}

		result, ok := res.(*tg.UploadFile)
		if !ok {
			return fmt.Errorf("unexpected type %T", res)
		}

		if err := os.WriteFile(destPath, result.Bytes, os.ModePerm); err != nil {
			return fmt.Errorf("Failed to write file: %w", err)
		}

		defer cleanCacheFile(destPath)

		logger.L.Infof("Downloaded file: %s", destPath)

		return saveFileWithRetry(task, destPath)
	}

	barTotalCount := calculateBarTotalCount(task.File.FileSize)

	progressCallback := func(bytesRead, contentLength int64) {
		progress := float64(bytesRead) / float64(contentLength) * 100
		logger.L.Tracef("Downloading %s: %.2f%%", task.String(), progress)
		if task.File.FileSize < 1024*1024*50 || int(progress)%(100/barTotalCount) != 0 {
			return
		}
		text := fmt.Sprintf("正在下载: %s\n[%s] %.2f%%",
			task.String(),
			getProgressBar(progress, barTotalCount),
			progress,
		)
		ctx.EditMessage(task.ChatID, &tg.MessagesEditMessageRequest{
			Message: text,
			ID:      task.ReplyMessageID,
		})
	}

	readCloser, err := NewTelegramReader(task.Ctx, bot.Client, &task.File.Location,
		0, task.File.FileSize-1, task.File.FileSize,
		progressCallback, task.File.FileSize/100)
	if err != nil {
		return fmt.Errorf("Failed to create reader: %w", err)
	}
	defer readCloser.Close()

	dest, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("Failed to create file: %w", err)
	}
	defer dest.Close()

	if _, err := io.CopyN(dest, readCloser, task.File.FileSize); err != nil {
		return fmt.Errorf("Failed to download file: %w", err)
	}

	defer cleanCacheFile(destPath)

	logger.L.Infof("Downloaded file: %s", destPath)
	ctx.EditMessage(task.ChatID, &tg.MessagesEditMessageRequest{
		Message: fmt.Sprintf("下载完成: %s\n正在转存文件...", task.FileName()),
		ID:      task.ReplyMessageID,
	})

	return saveFileWithRetry(task, destPath)
}

func worker(queue *queue.TaskQueue, semaphore chan struct{}) {
	for {
		semaphore <- struct{}{}
		task := queue.GetTask()
		logger.L.Debugf("Got task: %s", task.String())

		switch task.Status {
		case types.Pending:
			logger.L.Infof("Processing task: %s", task.String())
			if err := processPendingTask(&task); err != nil {
				logger.L.Errorf("Failed to do task: %s", err)
				task.Error = err
				if errors.Is(err, context.Canceled) {
					logger.L.Debugf("Task canceled: %s", task.String())
					task.Status = types.Canceled
				} else {
					task.Status = types.Failed
				}
			} else {
				task.Status = types.Succeeded
			}
			queue.AddTask(task)
		case types.Succeeded:
			logger.L.Infof("Task succeeded: %s", task.String())
			task.Ctx.(*ext.Context).EditMessage(task.ChatID, &tg.MessagesEditMessageRequest{
				Message: "保存成功\n" + task.FileName(),
				ID:      task.ReplyMessageID,
			})
		case types.Failed:
			logger.L.Errorf("Task failed: %s", task.String())
			task.Ctx.(*ext.Context).EditMessage(task.ChatID, &tg.MessagesEditMessageRequest{
				Message: "文件保存失败\n" + task.Error.Error(),
				ID:      task.ReplyMessageID,
			})
		case types.Canceled:
			logger.L.Infof("Task canceled: %s", task.String())
		default:
			logger.L.Errorf("Unknown task status: %s", task.Status)
		}
		<-semaphore
		logger.L.Debugf("Task done: %s", task.String())
	}
}

func Run() {
	logger.L.Info("Start processing tasks...")
	semaphore := make(chan struct{}, config.Cfg.Workers)
	for i := 0; i < config.Cfg.Workers; i++ {
		go worker(queue.Queue, semaphore)
	}

}
