package app

import (
	"os"
	"time"

	"github.com/nakabonne/tstorage"
	"github.com/robfig/cron/v3"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
	"github.com/talkincode/logsight/common/zaplog"
	"github.com/talkincode/logsight/common/zaplog/log"
	"github.com/talkincode/logsight/models"
)

var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

func (a *Application) initJob() {
	loc, _ := time.LoadLocation(a.appConfig.System.Location)
	a.sched = cron.New(cron.WithLocation(loc), cron.WithParser(cronParser))

	var err error
	_, err = a.sched.AddFunc("@every 30s", func() {
		go a.SchedSystemMonitorTask()
		go a.SchedProcessMonitorTask()
	})

	// database backup
	_, err = a.sched.AddFunc("@daily", func() {
		err := app.BackupDatabase()
		if err != nil {
			log.Errorf("database backup err %s", err.Error())
		}
	})

	_, err = a.sched.AddFunc("@daily", func() {
		a.gormDB.
			Where("opt_time < ? ", time.Now().
				Add(-time.Hour*24*365)).Delete(models.SysOprLog{})
	})

	if err != nil {
		log.Errorf("init job error %s", err.Error())
	}

	a.sched.Start()
}

// SchedSystemMonitorTask system monitor
func (a *Application) SchedSystemMonitorTask() {
	defer func() {
		if err := recover(); err != nil {
			log.Error(err)
		}
	}()

	timestamp := time.Now().Unix()

	var cpuuse float64
	_cpuuse, err := cpu.Percent(0, false)
	if err == nil && len(_cpuuse) > 0 {
		cpuuse = _cpuuse[0]
	}
	err = zaplog.TSDB().InsertRows([]tstorage.Row{
		{
			Metric: "system_cpuuse",
			DataPoint: tstorage.DataPoint{
				Value:     cpuuse,
				Timestamp: timestamp,
			},
		},
	})
	if err != nil {
		log.Error("add timeseries data error:", err.Error())
	}

	_meminfo, err := mem.VirtualMemory()
	var memuse uint64
	if err == nil {
		memuse = _meminfo.Used
	}

	err = zaplog.TSDB().InsertRows([]tstorage.Row{
		{
			Metric: "system_memuse",
			DataPoint: tstorage.DataPoint{
				Value:     float64(memuse),
				Timestamp: timestamp,
			},
		},
	})
	if err != nil {
		log.Error("add timeseries data error:", err.Error())
	}
}

// SchedProcessMonitorTask app process monitor
func (a *Application) SchedProcessMonitorTask() {
	defer func() {
		if err := recover(); err != nil {
			log.Error(err)
		}
	}()

	timestamp := time.Now().Unix()

	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return
	}

	cpuuse, err := p.CPUPercent()
	if err != nil {
		cpuuse = 0
	}

	err = zaplog.TSDB().InsertRows([]tstorage.Row{
		{
			Metric: "app_cpuuse",
			DataPoint: tstorage.DataPoint{
				Value:     cpuuse,
				Timestamp: timestamp,
			},
		},
	})
	if err != nil {
		log.Error("add timeseries data error:", err.Error())
	}

	meminfo, err := p.MemoryInfo()
	if err != nil {
		return
	}
	memuse := meminfo.RSS / 1024 / 1024

	err = zaplog.TSDB().InsertRows([]tstorage.Row{
		{
			Metric: "app_memuse",
			DataPoint: tstorage.DataPoint{
				Value:     float64(memuse),
				Timestamp: timestamp,
			},
		},
	})
	if err != nil {
		log.Error("add timeseries data error:", err.Error())
	}
}
