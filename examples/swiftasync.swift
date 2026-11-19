// Swift concurrency guest: async functions, task groups, an actor, sleeping, and a continuation
// resumed from a dispatch queue, all on the cooperative thread pool. Needs macOS 12.
import Dispatch

actor Counter {
    var total = 0
    func add(_ n: Int) { total += n }
}

func square(_ n: Int) async -> Int {
    await Task.yield()
    return n * n
}

@main
struct Main {
    static func main() async {
        setvbuf(stdout, nil, _IONBF, 0)
        let counter = Counter()
        let squares = await withTaskGroup(of: Int.self) { group -> Int in
            for n in 1...100 {
                group.addTask {
                    await counter.add(n)
                    return await square(n)
                }
            }
            var sum = 0
            for await value in group { sum += value }
            return sum
        }
        print("group    100 tasks: squares \(squares) actor \(await counter.total)")

        async let a = square(20)
        async let b = square(22)
        print("let      \(await a + b)")

        let started = DispatchTime.now()
        try? await Task.sleep(nanoseconds: 50_000_000)
        let slept = DispatchTime.now().uptimeNanoseconds - started.uptimeNanoseconds
        print("sleep    at least 50 ms \(slept >= 50_000_000), under 5 s \(slept < 5_000_000_000)")

        let resumed: Int = await withCheckedContinuation { continuation in
            DispatchQueue.global().async { continuation.resume(returning: 7) }
        }
        print("resume   \(resumed)")

        let detached = Task.detached(priority: .background) { await square(9) }
        print("detached \(await detached.value)")

        let cancelled = Task { () -> Bool in
            try? await Task.sleep(nanoseconds: 10_000_000_000)
            return Task.isCancelled
        }
        cancelled.cancel()
        print("cancel   \(await cancelled.value)")
    }
}
