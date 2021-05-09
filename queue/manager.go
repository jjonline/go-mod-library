/*
 * @Time   : 2021/1/16 下午12:30
 * @Email  : jjonline@jjonline.cn
 */
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// *************************************************
// 队列管理者
// 1、实际维护已注册的任务类
// 2、维护管理工作进程worker
// 3、队列相关管控功能实现：启动、优雅停止、协程并发调度等
// *************************************************

// shutdownPollIntervalMax 优雅关闭进程最大重复尝试间隔时长
const shutdownPollIntervalMax = 500 * time.Millisecond

// ErrQueueClosed 队列处于优雅关闭或关闭状态错误
var ErrQueueClosed = errors.New("queue: Queue closed")

// ErrMaxAttemptsExceeded 尝试执行次数超限
var ErrMaxAttemptsExceeded = errors.New("queue: max execute attempts")

type atomicBool int32

func (b *atomicBool) isSet() bool { return atomic.LoadInt32((*int32)(b)) != 0 }
func (b *atomicBool) setTrue()    { atomic.StoreInt32((*int32)(b), 1) }
func (b *atomicBool) setFalse()   { atomic.StoreInt32((*int32)(b), 0) }

// manager 队列管理者，队列的调度执行和管理
type manager struct {
	queue            QueueIFace            // 队列底层实现实例
	channel          chan JobIFace         // 任务类执行job的通道chan
	logger           *zap.Logger           // zap logger
	concurrent       int64                 // 单个队列最大并发worker数
	tasks            map[string]TaskIFace  // 队列名与任务类实例映射map，interface无需显式指定执指针类型，但实际传参需指针类型
	failedJobHandler FailedJobHandler      // 失败任务[最大尝试次数后仍然尝试失败（Execute返回了Error 或 执行导致panic）的任务]处理器
	lock             sync.Mutex            // 并发锁
	doneChan         chan struct{}         // 关闭队列的信号控制chan
	inShutdown       atomicBool            // 原子态标记：是否处于优雅关闭状态中
	inWorkingMap     map[string]int64      // 当前正work中的jobID与workerID映射map
	workerStatus     map[int64]*atomicBool // worker工作进程状态标记map
	jitter           time.Duration         // 循环器抖动间隔
}

// newManager 实例化一个manager
// @param queue      队列实现底层实例指针
// @param logger     zap日志实例
// @param concurrent 队列实际执行并发worker工作者数量
func newManager(queue QueueIFace, logger *zap.Logger, concurrent int64) *manager {
	return &manager{
		queue:        queue,
		channel:      make(chan JobIFace), // no buffer channel, execute when worker received
		logger:       logger,
		concurrent:   concurrent,
		tasks:        make(map[string]TaskIFace),
		workerStatus: make(map[int64]*atomicBool, concurrent),
		inWorkingMap: make(map[string]int64),
		lock:         sync.Mutex{},
		jitter:       450 * time.Millisecond,
	}
}

// bootstrapOne 脚手架辅助载入注册一个任务类
func (m *manager) bootstrapOne(task TaskIFace) error {
	m.lock.Lock()

	// log
	m.logger.Debug(
		"bootstrap",
		zap.String("name", task.Name()),
		zap.Int64("max_tries", task.MaxTries()),
		zap.Int64("retry_interval", task.RetryInterval()),
	)

	m.tasks[task.Name()] = task
	m.lock.Unlock()

	return nil
}

// bootstrap 脚手架辅助载入注册多个任务类
func (m *manager) bootstrap(tasks []TaskIFace) (err error) {
	for _, job := range tasks {
		if err = m.bootstrapOne(job); nil != err {
			return err
		}
	}
	return nil
}

// start 启动队列进程工作者
func (m *manager) start() (err error) {
	// 队列处于关闭中状态时启动直接返回Err
	if m.shuttingDown() {
		return ErrQueueClosed
	}

	// 启动loop执行者循环调度
	go m.startLooper()

	// 并发启动多个消费worker进程
	var i int64
	for i = 0; i < m.concurrent; i++ {
		go m.startWorker(i)
	}

	return err
}

// startLooper 启动队列进程looper，循环触发job消费
func (m *manager) startLooper() {
	for {
		select {
		case <-m.getDoneChan():
			m.logger.Info("shutdown, queue looper exited")
			close(m.channel) // close job chan
			return
		default:
			m.looper() // continue loop all queue jobs
		}
	}
}

// looper 轮询 && 速率控制所有队列的looper
func (m *manager) looper() {
	// map的range是无序的，无需再随机pop队列
	// range本身就是随机的
	needSleep := true
	for name := range m.tasks {
		if job, exist := m.queue.Pop(name); exist {
			m.channel <- job // push job to worker for control process
			needSleep = false
		}
	}

	// 所有队列都没job任务 looper随机休眠
	if needSleep {
		m.logger.Debug("no job pop, sleep for a while")

		time.Sleep(m.looperJitter())
	}
}

// startWorker 启动队列进程工作者
func (m *manager) startWorker(workerID int64) {
	defer func() {
		m.logger.Info(fmt.Sprintf("queue worker-%d exited", workerID), zap.Int64("worker_id", workerID))
	}()

	// started logger
	m.logger.Info(fmt.Sprintf("queue worker-%d started", workerID), zap.Int64("worker_id", workerID))

	// 阻塞消费job chan
	for job := range m.channel {
		m.runJob(job, workerID) // process run job
	}
}

// runJob 执行队列job，超时控制 && 尝试次数控制，执行结果控制
func (m *manager) runJob(job JobIFace, workerID int64) {
	// set worker is true
	m.setWorkerStatus(workerID, true)

	// step1、任务类执行捕获可能的panic
	defer func() {
		// set worker execute is false
		m.setWorkerStatus(workerID, false)

		// delete in running map
		delete(m.inWorkingMap, job.Payload().ID)

		// recovery if panic
		if err := recover(); err != nil {
			m.logger.Error(
				"queue.execute.panic",
				zap.StackSkip("stack", 2),
				zap.String("queue", job.GetName()),
				zap.Int64("worker_id", workerID),
				zap.Any("payload", job.Payload()),
				zap.Any("error", err),
			)

			// 检查任务尝试执行次数 & 标记失败状态
			m.markJobAsFailedIfWillExceedMaxAttempts(job)
		}
	}()

	task, ok := m.tasks[job.GetName()]
	if !ok {
		return
	}

	// step2、因为没有超时主动退出机制当任务执行超时仍在执行时标记再次延迟
	if _, exist := m.inWorkingMap[job.Payload().ID]; exist {
		m.logger.Warn(
			"abort.for.waiting.prev.job.finish",
			zap.String("queue", job.GetName()),
			zap.Any("payload", job.Payload()),
			zap.Time("pop_time", job.PopTime()),
		)

		// 当前任务作为延迟任务再次投递
		// warning 当前正在执行的可能执行成功这样会导致一条任务多次被成功执行，需要任务类自主实现业务逻辑幂等
		if payload, err := json.Marshal(job.Payload()); err == nil {
			_ = job.Queue().Later(job.GetName(), maxExecuteDuration, payload)
		}
		return
	}

	// set in running map
	m.inWorkingMap[job.Payload().ID] = workerID

	// step3、检查任务尝试次数：超限标记任务失败后删除任务，未超限则执行
	if m.markJobAsFailedIfAlreadyExceedsMaxAttempts(job) {
		return
	}

	// step4、execute job task with timeout control
	m.logger.Info(
		"queue.job.processing",
		zap.String("queue", job.GetName()),
		zap.Int64("worker_id", workerID),
		zap.Any("payload", job.Payload()),
	)

	// timeout context control
	ctx, cancelFunc := context.WithTimeout(context.Background(), job.Timeout())
	defer cancelFunc()

	err := task.Execute(ctx, job.Payload().RawBody())
	if err == nil {
		// step5、任务类执行成功：删除任务即可
		m.logger.Info(
			"queue.job.processed",
			zap.String("queue", job.GetName()),
			zap.Int64("worker_id", workerID),
			zap.Any("payload", job.Payload()),
			zap.Duration("duration", time.Now().Sub(job.PopTime())),
		)
		_ = job.Delete()
	} else {
		// step6、任务类执行失败：依赖重试设置执行重试or最终执行失败处理
		m.logger.Warn(
			"queue.job.failed",
			zap.String("queue", job.GetName()),
			zap.Int64("worker_id", workerID),
			zap.Any("payload", job.Payload()),
			zap.Duration("duration", time.Now().Sub(job.PopTime())),
			zap.Error(err),
		)
		m.markJobAsFailedIfWillExceedMaxAttempts(job)
	}
}

// looperJitter looper循环器间隔抖动
func (m *manager) looperJitter() time.Duration {
	// looper最小为450毫秒间隔，最大为1000毫秒间隔
	base := 450 * time.Millisecond

	m.jitter = m.jitter + time.Duration(rand.Intn(int(base/3)))
	if m.jitter > 1*time.Second {
		m.jitter = base
	}

	return m.jitter
}

// markJobAsFailedIfAlreadyExceedsMaxAttempts job执行`之前`检测尝试次数是否超限
// 1、如果超限则方法体内部清理任务并返回true，表示该job需要停止执行
// 2、如果未超限则返回false
func (m *manager) markJobAsFailedIfAlreadyExceedsMaxAttempts(job JobIFace) (needSop bool) {
	// step1、执行时长检查，持续执行超过最大执行时长时记录日志
	if time.Now().Sub(job.PopTime()) >= maxExecuteDuration {
		m.logger.Warn(
			"execute.time.too.long",
			zap.String("queue", job.GetName()),
			zap.Any("payload", job.Payload()),
			zap.Time("pop_time", job.PopTime()),
		)
	}

	// step2、检查最大尝试次数
	if job.Attempts() <= job.Payload().MaxTries {
		return false
	}

	// step3、其他情况：执行job前检查就不通过，移除任务&&标记任务失败（最大尝试次数超过限制、持续执行超时、脏数据、意外中断的任务 等）
	m.failJob(job, ErrMaxAttemptsExceeded)

	return true
}

// markJobAsFailedIfWillExceedMaxAttempts job执行`之后`检测尝试次数是否超限
// 1、检查job执行是否超过基准时间以记录日志
// 2、检查job执行尝试次数
func (m *manager) markJobAsFailedIfWillExceedMaxAttempts(job JobIFace) {
	// step1、执行时长检查，持续执行超过最大执行时长时记录日志
	if time.Now().Sub(job.PopTime()) >= maxExecuteDuration {
		m.logger.Warn(
			"execute.time.too.long",
			zap.String("queue", job.GetName()),
			zap.Any("payload", job.Payload()),
			zap.Time("pop_time", job.PopTime()),
		)
	}

	// step2、检查最大尝试执行次数是否超限
	if job.Attempts() >= job.Payload().MaxTries {
		// 超过最大重试次数：本次执行失败 && 任务类最终执行失败 && delete任务
		m.failJob(job, ErrMaxAttemptsExceeded)
	} else {
		// 任务可以重试：本次执行失败 && 任务类还可以重试 && release任务
		_ = job.Release(job.Payload().RetryInterval)
	}
}

// failJob 失败的任务触发器
func (m *manager) failJob(job JobIFace, err error) {
	// -> 1、标记任务失败
	job.MarkAsFailed()

	// -> 2、任务状态未删除则删除任务
	if job.IsDeleted() {
		return
	}
	_ = job.Delete()

	// tag log
	m.logger.Error(
		"queue.fail",
		zap.String("queue", job.GetName()),
		zap.Any("payload", job.Payload()),
		zap.Error(err),
	)

	// -> 3、设置任务执行失败
	job.Failed(err)

	// -> 4、queue级别依赖是否有设置失败任务处理器动作
	if m.failedJobHandler != nil {
		_ = m.failedJobHandler(job.Payload(), err)
	}
}

// shutDown 优雅停止队列
// 1、停止轮询loop进程，不再投递job
// 2、上下文设置的等待超时时间内尽量允许执行中的job顺利完成，超时终止的 :reserved 有序队列将在下次执行时再次投递尝试执行
// @param ctx 超时上下文
func (m *manager) shutDown(ctx context.Context) (err error) {
	m.inShutdown.setTrue()

	// 关闭用于控制looper协程的`关闭chan`：这样looper就停止循环
	m.closeDoneChanLocked()

	// 优雅关闭等待时长逐步递增实现
	pollIntervalBase := time.Millisecond
	nextPollInterval := func() time.Duration {
		// Add 10% jitter.
		interval := pollIntervalBase + time.Duration(rand.Intn(int(pollIntervalBase/10)))
		// Double and clamp for next time.
		pollIntervalBase *= 2
		if pollIntervalBase > shutdownPollIntervalMax {
			pollIntervalBase = shutdownPollIntervalMax
		}
		return interval
	}

	m.logger.Info("try graceful shutdown queue, please wait seconds")

	timer := time.NewTimer(nextPollInterval())
	defer timer.Stop()
	for {
		if m.isWorkersDown() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			timer.Reset(nextPollInterval())
		}
	}
}

// getDoneChan 带初始化的获取关闭控制chan
func (m *manager) getDoneChan() <-chan struct{} {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.getDoneChanLocked()
}

// getDoneChanLocked 底层自动判断的初始化关闭控制chan
func (m *manager) getDoneChanLocked() chan struct{} {
	if m.doneChan == nil {
		m.doneChan = make(chan struct{})
	}
	return m.doneChan
}

// closeDoneChanLocked 关闭用于关闭控制的chan（继而发信号告诉looper和worker优雅停止）
func (m *manager) closeDoneChanLocked() {
	ch := m.getDoneChanLocked()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// setWorkerStatus 设置标记工作进程当前执行中 or 执行完毕
func (m *manager) setWorkerStatus(workerID int64, isRun bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	node, exist := m.workerStatus[workerID]
	if !exist {
		node = new(atomicBool)
		m.workerStatus[workerID] = node
	}

	if isRun {
		node.setTrue()
	} else {
		node.setFalse()
	}
}

// isWorkersDown 检查是否所有worker当前工作任务均处于down状态
func (m *manager) isWorkersDown() (down bool) {
	for _, node := range m.workerStatus {
		if node.isSet() {
			return false
		}
	}
	return true
}

// shuttingDown 检测当前队列是否处于正在关闭中的状态
func (m *manager) shuttingDown() bool {
	return m.inShutdown.isSet()
}
