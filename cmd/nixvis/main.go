package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/beyondxinxin/nixvis/internal/netparser"
	"github.com/beyondxinxin/nixvis/internal/stats"
	"github.com/beyondxinxin/nixvis/internal/storage"
	"github.com/beyondxinxin/nixvis/internal/util"
	"github.com/beyondxinxin/nixvis/internal/web"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/sirupsen/logrus"
)

func main() {
	if util.ProcessCliCommands() {
		os.Exit(0)
	}

	// 初始化日志、配置
	util.ConfigureLogging()

	logrus.Info("------ 服务启动成功 ------")
	// 在 main 函数中的适当位置添加
	logrus.Infof("构建时间: %s, Git提交: %s", util.BuildTime, util.GitCommit)
	defer logrus.Info("------ 服务已安全关闭 ------")

	// 初始化数据库
	err := netparser.InitIPGeoLocation()
	if err != nil {
		return
	}
	repository, err := initRepository()
	if err != nil {
		return
	}

	logParser := storage.NewLogParser(repository)
	statsFactory := stats.NewStatsFactory(repository)
	defer repository.Close()

	// 初始扫描
	initScan(logParser)

	// 启动HTTP服务器
	startHTTPServer(statsFactory)

	// 启动维护任务
	startPeriodicTaskScheduler(logParser)
}

// 初始化数据
func initRepository() (*storage.Repository, error) {
	logrus.Info("****** 1 初始化数据 ******")
	repository, err := storage.NewRepository()
	if err != nil {
		logrus.WithField("error", err).Error("Failed to create database file")
		return repository, err
	}

	if err := repository.Init(); err != nil {
		logrus.WithField("error", err).Error("Failed to create tables")
		return repository, err
	}

	return repository, nil
}

// 初始扫描
func initScan(parser *storage.LogParser) {
	logrus.Info("****** 2 初始扫描 ******")
	executePeriodicTasks(parser)
}

// 启动HTTP服务器
func startHTTPServer(statsFactory *stats.StatsFactory) {
	logrus.Info("****** 3 启动HTTP服务器 ******")
	cfg := util.ReadConfig()
	r := setupCORS(statsFactory)
	srv := &http.Server{
		Addr:    cfg.Server.Port,
		Handler: r,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			logrus.WithField("error", err).Error("Failed to start the server")
		}
	}()
	logrus.Info("服务器初始化成功")
}

// setupCORS 配置跨域中间件
func setupCORS(statsFactory *stats.StatsFactory) *gin.Engine {

	gin.DefaultWriter = logrus.StandardLogger().Writer()
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
	}))

	// 设置Web路由
	web.SetupRoutes(r, statsFactory)

	return r
}

// 启动维护任务
func startPeriodicTaskScheduler(logParser *storage.LogParser) {
	logrus.Info("****** 4 启动维护任务 ******")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runPeriodicTaskScheduler(ctx, logParser)

	// 等待程序退出
	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, os.Interrupt, syscall.SIGTERM)
	<-shutdownSignal

	logrus.Info("开始关闭服务 ......")

	cancel() // 取消上下文将会通知所有后台任务退出

	// 给后台任务一些时间来完成清理
	shutdownCtx, shutdownCancel :=
		context.WithTimeout(context.Background(), 1*time.Second)
	defer shutdownCancel()
	<-shutdownCtx.Done()
}

// runPeriodicTaskScheduler 运行周期性任务
func runPeriodicTaskScheduler(
	ctx context.Context, parser *storage.LogParser) {

	cfg := util.ReadConfig()
	interval := util.ParseInterval(cfg.System.TaskInterval, 5*time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	iteration := 0

	for {
		select {
		case <-ticker.C:
			iteration++
			logrus.WithFields(logrus.Fields{
				"iteration": iteration,
				"interval":  interval,
			}).Info("执行定期维护任务")

			executePeriodicTasks(parser)
		case <-ctx.Done():
			return
		}
	}
}

// executePeriodicTasks 执行周期性任务
func executePeriodicTasks(parser *storage.LogParser) {
	logrus.WithField("task", "nginx_scan").Info("开始扫描Nginx日志")

	startTime := time.Now()
	results := parser.ScanNginxLogs()
	totalDuration := time.Since(startTime)

	// 打印每个网站的扫描结果
	totalEntries := 0
	successCount := 0

	for _, result := range results {
		if result.WebName == "" {
			continue // 跳过空记录
		}

		totalEntries += result.TotalEntries

		if result.Success {
			successCount++
			logrus.Info(fmt.Sprintf("网站 %s (%s) 扫描完成: %d 条记录, 耗时 %.2fs",
				result.WebName, result.WebID, result.TotalEntries, result.Duration.Seconds()))
		} else {
			logrus.Info(fmt.Sprintf("网站 %s (%s) 扫描失败: %s",
				result.WebName, result.WebID, result.Error))
		}
	}

	// 任务完成总结日志
	logrus.Info(fmt.Sprintf("Nginx日志扫描完成: %d/%d 个站点成功, 共 %d 条记录, 总耗时 %.2fs",
		successCount, len(results), totalEntries, totalDuration.Seconds()))
}
