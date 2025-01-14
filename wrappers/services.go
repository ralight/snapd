// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package wrappers

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/osutil/sys"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/strutil"
	"github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/timeout"
	"github.com/snapcore/snapd/timeutil"
	"github.com/snapcore/snapd/timings"
)

type interacter interface {
	Notify(status string)
}

// wait this time between TERM and KILL
var killWait = 5 * time.Second

func serviceStopTimeout(app *snap.AppInfo) time.Duration {
	tout := app.StopTimeout
	if tout == 0 {
		tout = timeout.DefaultTimeout
	}
	return time.Duration(tout)
}

func generateSnapServiceFile(app *snap.AppInfo) ([]byte, error) {
	if err := snap.ValidateApp(app); err != nil {
		return nil, err
	}

	return genServiceFile(app), nil
}

func stopService(sysd systemd.Systemd, app *snap.AppInfo, inter interacter) error {
	serviceName := app.ServiceName()
	tout := serviceStopTimeout(app)

	stopErrors := []error{}
	for _, socket := range app.Sockets {
		if err := sysd.Stop(filepath.Base(socket.File()), tout); err != nil {
			stopErrors = append(stopErrors, err)
		}
	}

	if app.Timer != nil {
		if err := sysd.Stop(filepath.Base(app.Timer.File()), tout); err != nil {
			stopErrors = append(stopErrors, err)
		}
	}

	if err := sysd.Stop(serviceName, tout); err != nil {
		if !systemd.IsTimeout(err) {
			return err
		}
		inter.Notify(fmt.Sprintf("%s refused to stop, killing.", serviceName))
		// ignore errors for kill; nothing we'd do differently at this point
		sysd.Kill(serviceName, "TERM", "")
		time.Sleep(killWait)
		sysd.Kill(serviceName, "KILL", "")

	}

	if len(stopErrors) > 0 {
		return stopErrors[0]
	}

	return nil
}

// StartServices starts service units for the applications from the snap which
// are services. Service units will be started in the order provided by the
// caller.
func StartServices(apps []*snap.AppInfo, inter interacter, tm timings.Measurer) (err error) {
	sysd := systemd.New(dirs.GlobalRootDir, systemd.SystemMode, inter)

	services := make([]string, 0, len(apps))
	for _, app := range apps {
		// they're *supposed* to be all services, but checking doesn't hurt
		if !app.IsService() {
			continue
		}

		defer func(app *snap.AppInfo) {
			if err == nil {
				return
			}
			if e := stopService(sysd, app, inter); e != nil {
				inter.Notify(fmt.Sprintf("While trying to stop previously started service %q: %v", app.ServiceName(), e))
			}
			for _, socket := range app.Sockets {
				socketService := filepath.Base(socket.File())
				if e := sysd.Disable(socketService); e != nil {
					inter.Notify(fmt.Sprintf("While trying to disable previously enabled socket service %q: %v", socketService, e))
				}
			}
			if app.Timer != nil {
				timerService := filepath.Base(app.Timer.File())
				if e := sysd.Disable(timerService); e != nil {
					inter.Notify(fmt.Sprintf("While trying to disable previously enabled timer service %q: %v", timerService, e))
				}
			}
		}(app)

		if len(app.Sockets) == 0 && app.Timer == nil {
			// check if the service is disabled, if so don't start it up
			// this could happen for example if the service was disabled in
			// the install hook by snapctl or if the service was disabled in
			// the previous installation
			isEnabled, err := sysd.IsEnabled(app.ServiceName())
			if err != nil {
				return err
			}

			if isEnabled {
				services = append(services, app.ServiceName())
			}
		}

		for _, socket := range app.Sockets {
			socketService := filepath.Base(socket.File())
			// enable the socket
			if err := sysd.Enable(socketService); err != nil {
				return err
			}

			timings.Run(tm, "start-socket-service", fmt.Sprintf("start socket service %q", socketService), func(nested timings.Measurer) {
				err = sysd.Start(socketService)
			})
			if err != nil {
				return err
			}
		}

		if app.Timer != nil {
			timerService := filepath.Base(app.Timer.File())
			// enable the timer
			if err := sysd.Enable(timerService); err != nil {
				return err
			}

			timings.Run(tm, "start-timer-service", fmt.Sprintf("start timer service %q", timerService), func(nested timings.Measurer) {
				err = sysd.Start(timerService)
			})
			if err != nil {
				return err
			}
		}
	}

	for _, srv := range services {
		// starting all services at once does not create a single
		// transaction, but instead spawns multiple jobs, make sure the
		// services started in the original order by bring them up one
		// by one, see:
		// https://github.com/systemd/systemd/issues/8102
		// https://lists.freedesktop.org/archives/systemd-devel/2018-January/040152.html
		timings.Run(tm, "start-service", fmt.Sprintf("start service %q", srv), func(nested timings.Measurer) {
			err = sysd.Start(srv)
		})
		if err != nil {
			// cleanup was set up by iterating over apps
			return err
		}
	}

	return nil
}

// AddSnapServices adds service units for the applications from the snap which are services.
func AddSnapServices(s *snap.Info, disabledSvcs []string, inter interacter) (err error) {
	if s.GetType() == snap.TypeSnapd {
		return writeSnapdServicesOnCore(s, inter)
	}

	// check if any previously disabled services are now no longer services and
	// log messages about that
	for _, svc := range disabledSvcs {
		app, ok := s.Apps[svc]
		if !ok {
			logger.Noticef("previously disabled service %s no longer exists", svc)
		} else if !app.IsService() {
			logger.Noticef("previously disabled service %s is now an app and not a service", svc)
		}
	}

	sysd := systemd.New(dirs.GlobalRootDir, systemd.SystemMode, inter)
	var written []string
	var enabled []string
	defer func() {
		if err == nil {
			return
		}
		for _, s := range enabled {
			if e := sysd.Disable(s); e != nil {
				inter.Notify(fmt.Sprintf("while trying to disable %s due to previous failure: %v", s, e))
			}
		}
		for _, s := range written {
			if e := os.Remove(s); e != nil {
				inter.Notify(fmt.Sprintf("while trying to remove %s due to previous failure: %v", s, e))
			}
		}
		if len(written) > 0 {
			if e := sysd.DaemonReload(); e != nil {
				inter.Notify(fmt.Sprintf("while trying to perform systemd daemon-reload due to previous failure: %v", e))
			}
		}
	}()

	for _, app := range s.Apps {
		if !app.IsService() {
			continue
		}
		// Generate service file
		content, err := generateSnapServiceFile(app)
		if err != nil {
			return err
		}
		svcFilePath := app.ServiceFile()
		os.MkdirAll(filepath.Dir(svcFilePath), 0755)
		if err := osutil.AtomicWriteFile(svcFilePath, content, 0644, 0); err != nil {
			return err
		}
		written = append(written, svcFilePath)

		// Generate systemd .socket files if needed
		socketFiles, err := generateSnapSocketFiles(app)
		if err != nil {
			return err
		}
		for path, content := range *socketFiles {
			os.MkdirAll(filepath.Dir(path), 0755)
			if err := osutil.AtomicWriteFile(path, content, 0644, 0); err != nil {
				return err
			}
			written = append(written, path)
		}

		if app.Timer != nil {
			content, err := generateSnapTimerFile(app)
			if err != nil {
				return err
			}
			path := app.Timer.File()
			os.MkdirAll(filepath.Dir(path), 0755)
			if err := osutil.AtomicWriteFile(path, content, 0644, 0); err != nil {
				return err
			}
			written = append(written, path)
		}

		if app.Timer != nil || len(app.Sockets) != 0 {
			// service is socket or timer activated, not during the
			// boot
			continue
		}

		svcName := app.ServiceName()

		if strutil.ListContains(disabledSvcs, app.Name) {
			// service is disabled, nothing to do
			continue
		}

		if err := sysd.Enable(svcName); err != nil {
			return err
		}
		enabled = append(enabled, svcName)
	}

	if len(written) > 0 {
		if err := sysd.DaemonReload(); err != nil {
			return err
		}
	}

	return nil
}

// StopServices stops service units for the applications from the snap which are services.
func StopServices(apps []*snap.AppInfo, reason snap.ServiceStopReason, inter interacter, tm timings.Measurer) error {
	sysd := systemd.New(dirs.GlobalRootDir, systemd.SystemMode, inter)

	logger.Debugf("StopServices called for %q, reason: %v", apps, reason)
	for _, app := range apps {
		// Handle the case where service file doesn't exist and don't try to stop it as it will fail.
		// This can happen with snap try when snap.yaml is modified on the fly and a daemon line is added.
		if !app.IsService() || !osutil.FileExists(app.ServiceFile()) {
			continue
		}
		// Skip stop on refresh when refresh mode is set to something
		// other than "restart" (or "" which is the same)
		if reason == snap.StopReasonRefresh {
			logger.Debugf(" %s refresh-mode: %v", app.Name, app.StopMode)
			switch app.RefreshMode {
			case "endure":
				// skip this service
				continue
			}
		}

		var err error
		timings.Run(tm, "stop-service", fmt.Sprintf("stop service %q", app.ServiceName()), func(nested timings.Measurer) {
			err = stopService(sysd, app, inter)
		})
		if err != nil {
			return err
		}

		// ensure the service is really stopped on remove regardless
		// of stop-mode
		if reason == snap.StopReasonRemove && !app.StopMode.KillAll() {
			// FIXME: make this smarter and avoid the killWait
			//        delay if not needed (i.e. if all processes
			//        have died)
			sysd.Kill(app.ServiceName(), "TERM", "all")
			time.Sleep(killWait)
			sysd.Kill(app.ServiceName(), "KILL", "")
		}
	}

	return nil
}

// ServicesEnableState returns a map of service names from the given snap,
// together with their enable/disable status.
func ServicesEnableState(s *snap.Info, inter interacter) (map[string]bool, error) {
	sysd := systemd.New(dirs.GlobalRootDir, systemd.SystemMode, inter)

	// loop over all services in the snap, querying systemd for the current
	// systemd state of the snaps
	snapSvcsState := make(map[string]bool, len(s.Apps))
	for name, app := range s.Apps {
		if !app.IsService() {
			continue
		}
		state, err := sysd.IsEnabled(app.ServiceName())
		if err != nil {
			return nil, err
		}
		snapSvcsState[name] = state
	}
	return snapSvcsState, nil
}

// RemoveSnapServices disables and removes service units for the applications from the snap which are services.
func RemoveSnapServices(s *snap.Info, inter interacter) error {
	sysd := systemd.New(dirs.GlobalRootDir, systemd.SystemMode, inter)
	nservices := 0

	for _, app := range s.Apps {
		if !app.IsService() || !osutil.FileExists(app.ServiceFile()) {
			continue
		}
		nservices++

		serviceName := filepath.Base(app.ServiceFile())

		for _, socket := range app.Sockets {
			path := socket.File()
			socketServiceName := filepath.Base(path)
			if err := sysd.Disable(socketServiceName); err != nil {
				return err
			}

			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				logger.Noticef("Failed to remove socket file %q for %q: %v", path, serviceName, err)
			}
		}

		if app.Timer != nil {
			path := app.Timer.File()

			timerName := filepath.Base(path)
			if err := sysd.Disable(timerName); err != nil {
				return err
			}

			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				logger.Noticef("Failed to remove timer file %q for %q: %v", path, serviceName, err)
			}
		}

		if err := sysd.Disable(serviceName); err != nil {
			return err
		}

		if err := os.Remove(app.ServiceFile()); err != nil && !os.IsNotExist(err) {
			logger.Noticef("Failed to remove service file for %q: %v", serviceName, err)
		}

	}

	// only reload if we actually had services
	if nservices > 0 {
		if err := sysd.DaemonReload(); err != nil {
			return err
		}
	}

	return nil
}

func genServiceNames(snap *snap.Info, appNames []string) []string {
	names := make([]string, 0, len(appNames))

	for _, name := range appNames {
		if app := snap.Apps[name]; app != nil {
			names = append(names, app.ServiceName())
		}
	}
	return names
}

func genServiceFile(appInfo *snap.AppInfo) []byte {
	serviceTemplate := `[Unit]
# Auto-generated, DO NOT EDIT
Description=Service for snap application {{.App.Snap.InstanceName}}.{{.App.Name}}
Requires={{.MountUnit}}
Wants={{.PrerequisiteTarget}}
After={{.MountUnit}} {{.PrerequisiteTarget}}{{if .After}} {{ stringsJoin .After " " }}{{end}}
{{- if .Before}}
Before={{ stringsJoin .Before " "}}
{{- end}}
X-Snappy=yes

[Service]
ExecStart={{.App.LauncherCommand}}
SyslogIdentifier={{.App.Snap.InstanceName}}.{{.App.Name}}
Restart={{.Restart}}
{{- if .App.RestartDelay}}
RestartSec={{.App.RestartDelay.Seconds}}
{{- end}}
WorkingDirectory={{.App.Snap.DataDir}}
{{- if .App.StopCommand}}
ExecStop={{.App.LauncherStopCommand}}
{{- end}}
{{- if .App.ReloadCommand}}
ExecReload={{.App.LauncherReloadCommand}}
{{- end}}
{{- if .App.PostStopCommand}}
ExecStopPost={{.App.LauncherPostStopCommand}}
{{- end}}
{{- if .StopTimeout}}
TimeoutStopSec={{.StopTimeout.Seconds}}
{{- end}}
{{- if .StartTimeout}}
TimeoutStartSec={{.StartTimeout.Seconds}}
{{- end}}
Type={{.App.Daemon}}
{{- if .Remain}}
RemainAfterExit={{.Remain}}
{{- end}}
{{- if .App.BusName}}
BusName={{.App.BusName}}
{{- end}}
{{- if .App.WatchdogTimeout}}
WatchdogSec={{.App.WatchdogTimeout.Seconds}}
{{- end}}
{{- if .KillMode}}
KillMode={{.KillMode}}
{{- end}}
{{- if .KillSignal}}
KillSignal={{.KillSignal}}
{{- end}}
{{- if not .App.Sockets}}

[Install]
WantedBy={{.ServicesTarget}}
{{- end}}
`
	var templateOut bytes.Buffer
	tmpl := template.New("service-wrapper")
	tmpl.Funcs(template.FuncMap{
		"stringsJoin": strings.Join,
	})
	t := template.Must(tmpl.Parse(serviceTemplate))

	restartCond := appInfo.RestartCond.String()
	if restartCond == "" {
		restartCond = snap.RestartOnFailure.String()
	}

	var remain string
	if appInfo.Daemon == "oneshot" {
		// any restart condition other than "no" is invalid for oneshot daemons
		restartCond = "no"
		// If StopExec is present for a oneshot service than we also need
		// RemainAfterExit=yes
		if appInfo.StopCommand != "" {
			remain = "yes"
		}
	}
	var killMode string
	if !appInfo.StopMode.KillAll() {
		killMode = "process"
	}

	wrapperData := struct {
		App *snap.AppInfo

		Restart            string
		StopTimeout        time.Duration
		StartTimeout       time.Duration
		ServicesTarget     string
		PrerequisiteTarget string
		MountUnit          string
		Remain             string
		KillMode           string
		KillSignal         string
		Before             []string
		After              []string

		Home    string
		EnvVars string
	}{
		App: appInfo,

		Restart:            restartCond,
		StopTimeout:        serviceStopTimeout(appInfo),
		StartTimeout:       time.Duration(appInfo.StartTimeout),
		ServicesTarget:     systemd.ServicesTarget,
		PrerequisiteTarget: systemd.PrerequisiteTarget,
		MountUnit:          filepath.Base(systemd.MountUnitPath(appInfo.Snap.MountDir())),
		Remain:             remain,
		KillMode:           killMode,
		KillSignal:         appInfo.StopMode.KillSignal(),

		Before: genServiceNames(appInfo.Snap, appInfo.Before),
		After:  genServiceNames(appInfo.Snap, appInfo.After),

		// systemd runs as PID 1 so %h will not work.
		Home: "/root",
	}

	if err := t.Execute(&templateOut, wrapperData); err != nil {
		// this can never happen, except we forget a variable
		logger.Panicf("Unable to execute template: %v", err)
	}

	return templateOut.Bytes()
}

func genServiceSocketFile(appInfo *snap.AppInfo, socketName string) []byte {
	socketTemplate := `[Unit]
# Auto-generated, DO NOT EDIT
Description=Socket {{.SocketName}} for snap application {{.App.Snap.InstanceName}}.{{.App.Name}}
Requires={{.MountUnit}}
After={{.MountUnit}}
X-Snappy=yes

[Socket]
Service={{.ServiceFileName}}
FileDescriptorName={{.SocketInfo.Name}}
ListenStream={{.ListenStream}}
{{- if .SocketInfo.SocketMode}}
SocketMode={{.SocketInfo.SocketMode | printf "%04o"}}
{{- end}}

[Install]
WantedBy={{.SocketsTarget}}
`
	var templateOut bytes.Buffer
	t := template.Must(template.New("socket-wrapper").Parse(socketTemplate))

	socket := appInfo.Sockets[socketName]
	listenStream := renderListenStream(socket)
	wrapperData := struct {
		App             *snap.AppInfo
		ServiceFileName string
		SocketsTarget   string
		MountUnit       string
		SocketName      string
		SocketInfo      *snap.SocketInfo
		ListenStream    string
	}{
		App:             appInfo,
		ServiceFileName: filepath.Base(appInfo.ServiceFile()),
		SocketsTarget:   systemd.SocketsTarget,
		MountUnit:       filepath.Base(systemd.MountUnitPath(appInfo.Snap.MountDir())),
		SocketName:      socketName,
		SocketInfo:      socket,
		ListenStream:    listenStream,
	}

	if err := t.Execute(&templateOut, wrapperData); err != nil {
		// this can never happen, except we forget a variable
		logger.Panicf("Unable to execute template: %v", err)
	}

	return templateOut.Bytes()
}

func generateSnapSocketFiles(app *snap.AppInfo) (*map[string][]byte, error) {
	if err := snap.ValidateApp(app); err != nil {
		return nil, err
	}

	socketFiles := make(map[string][]byte)
	for name, socket := range app.Sockets {
		socketFiles[socket.File()] = genServiceSocketFile(app, name)
	}
	return &socketFiles, nil
}

func renderListenStream(socket *snap.SocketInfo) string {
	snap := socket.App.Snap
	listenStream := strings.Replace(socket.ListenStream, "$SNAP_DATA", snap.DataDir(), -1)
	// TODO: when we support User/Group in the generated systemd unit,
	// adjust this accordingly
	serviceUserUid := sys.UserID(0)
	runtimeDir := snap.UserXdgRuntimeDir(serviceUserUid)
	listenStream = strings.Replace(listenStream, "$XDG_RUNTIME_DIR", runtimeDir, -1)
	return strings.Replace(listenStream, "$SNAP_COMMON", snap.CommonDataDir(), -1)
}

func generateSnapTimerFile(app *snap.AppInfo) ([]byte, error) {
	timerTemplate := `[Unit]
# Auto-generated, DO NOT EDIT
Description=Timer {{.TimerName}} for snap application {{.App.Snap.InstanceName}}.{{.App.Name}}
Requires={{.MountUnit}}
After={{.MountUnit}}
X-Snappy=yes

[Timer]
Unit={{.ServiceFileName}}
{{ range .Schedules }}OnCalendar={{ . }}
{{ end }}
[Install]
WantedBy={{.TimersTarget}}
`
	var templateOut bytes.Buffer
	t := template.Must(template.New("timer-wrapper").Parse(timerTemplate))

	timerSchedule, err := timeutil.ParseSchedule(app.Timer.Timer)
	if err != nil {
		return nil, err
	}

	schedules := generateOnCalendarSchedules(timerSchedule)

	wrapperData := struct {
		App             *snap.AppInfo
		ServiceFileName string
		TimersTarget    string
		TimerName       string
		MountUnit       string
		Schedules       []string
	}{
		App:             app,
		ServiceFileName: filepath.Base(app.ServiceFile()),
		TimersTarget:    systemd.TimersTarget,
		TimerName:       app.Name,
		MountUnit:       filepath.Base(systemd.MountUnitPath(app.Snap.MountDir())),
		Schedules:       schedules,
	}

	if err := t.Execute(&templateOut, wrapperData); err != nil {
		// this can never happen, except we forget a variable
		logger.Panicf("Unable to execute template: %v", err)
	}

	return templateOut.Bytes(), nil

}

func makeAbbrevWeekdays(start time.Weekday, end time.Weekday) []string {
	out := make([]string, 0, 7)
	for w := start; w%7 != (end + 1); w++ {
		out = append(out, time.Weekday(w % 7).String()[0:3])
	}
	return out
}

// generateOnCalendarSchedules converts a schedule into OnCalendar schedules
// suitable for use in systemd *.timer units using systemd.time(7)
// https://www.freedesktop.org/software/systemd/man/systemd.time.html
func generateOnCalendarSchedules(schedule []*timeutil.Schedule) []string {
	calendarEvents := make([]string, 0, len(schedule))
	for _, sched := range schedule {
		days := make([]string, 0, len(sched.WeekSpans))
		for _, week := range sched.WeekSpans {
			abbrev := strings.Join(makeAbbrevWeekdays(week.Start.Weekday, week.End.Weekday), ",")
			switch week.Start.Pos {
			case timeutil.EveryWeek:
				// eg: mon, mon-fri, fri-mon
				days = append(days, fmt.Sprintf("%s *-*-*", abbrev))
			case timeutil.LastWeek:
				// eg: mon5
				days = append(days, fmt.Sprintf("%s *-*~7/1", abbrev))
			default:
				// eg: mon1, fri1, mon1-tue2
				startDay := (week.Start.Pos-1)*7 + 1
				endDay := week.End.Pos * 7

				// NOTE: schedule mon1-tue2 (all weekdays
				// between the first Monday of the month, until
				// the second Tuesday of the month) is not
				// translatable to systemd.time(7) format, for
				// this assume all weekdays and allow the runner
				// to do the filtering
				if week.Start != week.End {
					days = append(days,
						fmt.Sprintf("*-*-%d..%d/1", startDay, endDay))
				} else {
					days = append(days,
						fmt.Sprintf("%s *-*-%d..%d/1", abbrev, startDay, endDay))
				}

			}
		}

		if len(days) == 0 {
			// no weekday spec, meaning the timer runs every day
			days = []string{"*-*-*"}
		}

		startTimes := make([]string, 0, len(sched.ClockSpans))
		for _, clocks := range sched.ClockSpans {
			// use expanded clock spans
			for _, span := range clocks.ClockSpans() {
				when := span.Start
				if span.Spread {
					length := span.End.Sub(span.Start)
					if length < 0 {
						// span Start wraps around, so we have '00:00.Sub(23:45)'
						length = -length
					}
					if length > 5*time.Minute {
						// replicate what timeutil.Next() does
						// and cut some time at the end of the
						// window so that events do not happen
						// directly one after another
						length -= 5 * time.Minute
					}
					when = when.Add(time.Duration(rand.Int63n(int64(length))))
				}
				if when.Hour == 24 {
					// 24:00 for us means the other end of
					// the day, for systemd we need to
					// adjust it to the 0-23 hour range
					when.Hour -= 24
				}

				startTimes = append(startTimes, when.String())
			}
		}

		for _, day := range days {
			for _, startTime := range startTimes {
				calendarEvents = append(calendarEvents, fmt.Sprintf("%s %s", day, startTime))
			}
		}
	}
	return calendarEvents
}
