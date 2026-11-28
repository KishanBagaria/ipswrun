// Threads guest: POSIX threads, locks and condition variables, thread-local storage, and
// libdispatch queues, groups, semaphores, timers and once. Each line is printed as its step
// finishes, so a run that stops says how far it got.
#include <dispatch/dispatch.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

#define THREADS 4
#define ROUNDS 20000

static pthread_mutex_t lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t ready = PTHREAD_COND_INITIALIZER;
static long counter, turn;
static atomic_long atom;
static __thread long local;
static pthread_key_t key;
static atomic_int destroyed;

static void destroy(void *value) { atomic_fetch_add(&destroyed, 1); }

static void *count(void *arg) {
    pthread_setspecific(key, arg);
    for (int i = 0; i < ROUNDS; i++) {
        pthread_mutex_lock(&lock);
        counter++;
        pthread_mutex_unlock(&lock);
        atomic_fetch_add(&atom, 1);
        local++;
    }
    return (void *)(local + (pthread_getspecific(key) == arg));
}

// Threads take turns in order: each waits on the condition variable until it is its turn.
static void *take_turn(void *arg) {
    long me = (long)arg;
    pthread_mutex_lock(&lock);
    while (turn != me) pthread_cond_wait(&ready, &lock);
    turn++;
    pthread_cond_broadcast(&ready);
    pthread_mutex_unlock(&lock);
    return NULL;
}

static void *sleeper(void *arg) {
    usleep(20000);
    return (void *)(long)pthread_main_np();
}

// A lock held for writing while readers and another writer arrive: they all have to wait.
static pthread_rwlock_t contended = PTHREAD_RWLOCK_INITIALIZER;
static long guarded;

static void *read_guarded(void *arg) {
    pthread_rwlock_rdlock(&contended);
    long value = guarded;
    usleep(1000);
    pthread_rwlock_unlock(&contended);
    return (void *)value;
}

static void *write_guarded(void *arg) {
    pthread_rwlock_wrlock(&contended);
    long value = guarded;
    usleep(1000);
    guarded = value + 10;
    pthread_rwlock_unlock(&contended);
    return NULL;
}

static void once(void *context) { ++*(int *)context; }

int main(void) {
    setvbuf(stdout, NULL, _IONBF, 0);
    pthread_t threads[THREADS];
    pthread_key_create(&key, destroy);

    long sum = 0;
    for (long i = 0; i < THREADS; i++) pthread_create(&threads[i], NULL, count, (void *)(i + 1));
    for (int i = 0; i < THREADS; i++) {
        void *result;
        pthread_join(threads[i], &result);
        sum += (long)result;
    }
    printf("pthread  %d threads: mutex %ld atomic %ld thread-local %ld destructors %d\n", THREADS, counter, atomic_load(&atom),
           sum, atomic_load(&destroyed));

    for (long i = 0; i < THREADS; i++) pthread_create(&threads[i], NULL, take_turn, (void *)(THREADS - 1 - i));
    for (int i = 0; i < THREADS; i++) pthread_join(threads[i], NULL);
    printf("cond     %ld turns in order\n", turn);

    void *was_main;
    pthread_create(&threads[0], NULL, sleeper, NULL);
    pthread_join(threads[0], &was_main);
    printf("sleep    joined a sleeping thread, main there %ld here %d\n", (long)was_main, pthread_main_np());

    pthread_rwlock_t rw = PTHREAD_RWLOCK_INITIALIZER;
    pthread_rwlock_rdlock(&rw);
    pthread_rwlock_rdlock(&rw);
    int busy = pthread_rwlock_trywrlock(&rw) != 0;
    pthread_rwlock_unlock(&rw);
    pthread_rwlock_unlock(&rw);
    printf("rwlock   writer kept out by readers %d, then let in %d\n", busy, pthread_rwlock_wrlock(&rw) == 0);

    pthread_t waiters[6];
    pthread_rwlock_wrlock(&contended);
    guarded = 1;
    for (int i = 0; i < 6; i++) pthread_create(&waiters[i], NULL, i == 3 ? write_guarded : read_guarded, NULL);
    usleep(20000);
    pthread_rwlock_unlock(&contended);
    int consistent = 1;
    for (int i = 0; i < 6; i++) {
        void *value;
        pthread_join(waiters[i], &value);
        consistent &= i == 3 || (long)value == 1 || (long)value == 11;
    }
    printf("rwlock   5 readers and a writer waited: readers consistent %d, value %ld\n", consistent, guarded);

    dispatch_queue_t global = dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0);
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    __block int ran = 0, off_main = 0;
    dispatch_async(global, ^{
        ran = 1;
        off_main = !pthread_main_np();
        dispatch_semaphore_signal(done);
    });
    long timed_out = dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));
    printf("async    ran %d off the main thread %d timed out %ld\n", ran, off_main, timed_out);

    dispatch_group_t group = dispatch_group_create();
    __block atomic_long total = 0;
    for (long i = 1; i <= 100; i++) dispatch_group_async(group, global, ^{ atomic_fetch_add(&total, i); });
    timed_out = dispatch_group_wait(group, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));
    printf("group    100 blocks sum %ld timed out %ld\n", atomic_load(&total), timed_out);

    dispatch_queue_t serial = dispatch_queue_create("ipswrun.serial", DISPATCH_QUEUE_SERIAL);
    __block long order = 0, in_order = 1;
    for (long i = 0; i < 50; i++) dispatch_async(serial, ^{ in_order &= order++ == i; });
    dispatch_sync(serial, ^{ in_order &= order == 50; });
    printf("serial   50 blocks in order %ld\n", in_order);

    __block atomic_long applied = 0;
    dispatch_apply(64, global, ^(size_t i) { atomic_fetch_add(&applied, i); });
    printf("apply    64 iterations sum %ld\n", atomic_load(&applied));

    static dispatch_once_t token;
    int calls = 0;
    dispatch_once_f(&token, &calls, once);
    dispatch_once_f(&token, &calls, once);
    printf("once     %d call\n", calls);

    __block int fired = 0;
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 50 * NSEC_PER_MSEC), global, ^{
        fired = 1;
        dispatch_semaphore_signal(done);
    });
    timed_out = dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));
    printf("after    fired %d timed out %ld\n", fired, timed_out);

    dispatch_source_t timer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, serial);
    __block int ticks = 0;
    dispatch_source_set_timer(timer, dispatch_time(DISPATCH_TIME_NOW, 0), 10 * NSEC_PER_MSEC, 0);
    dispatch_source_set_event_handler(timer, ^{
        if (++ticks == 3) {
            dispatch_source_cancel(timer);
            dispatch_semaphore_signal(done);
        }
    });
    dispatch_resume(timer);
    timed_out = dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));
    printf("timer    3 ticks %d timed out %ld\n", ticks == 3, timed_out);

    // The main queue is drained by dispatch_main, which never returns: the last block exits.
    dispatch_async(global, ^{
        dispatch_async(dispatch_get_main_queue(), ^{
            printf("main     block on the main thread %d\n", pthread_main_np());
            exit(0);
        });
    });
    dispatch_main();
}
