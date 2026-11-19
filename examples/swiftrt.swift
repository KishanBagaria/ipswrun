// Swift runtime guest: generics, protocols, existentials, reflection, Codable, strings, errors.
import Foundation

protocol Shape { var area: Double { get } }
struct Circle: Shape, Codable, Hashable { var r: Double; var area: Double { Double.pi * r * r } }
struct Rect: Shape, Codable, Hashable { var w, h: Double; var area: Double { w * h } }
enum Tree<T: Comparable> {
    case leaf
    indirect case node(Tree<T>, T, Tree<T>)
    func inserting(_ x: T) -> Tree<T> {
        switch self {
        case .leaf: return .node(.leaf, x, .leaf)
        case let .node(l, v, r): return x < v ? .node(l.inserting(x), v, r) : .node(l, v, r.inserting(x))
        }
    }
    var sorted: [T] {
        switch self {
        case .leaf: return []
        case let .node(l, v, r): return l.sorted + [v] + r.sorted
        }
    }
}
final class Counter { var n = 0; deinit { print("deinit after \(n)") } }
enum Failure: Error { case tooBig(Int) }
struct Doc: Codable { var name: String; var tags: [String]; var shapes: [Circle]; var when: Date; var extra: [String: Int]? }

func check(_ n: Int) throws -> Int { if n > 3 { throw Failure.tooBig(n) }; return n }

let shapes: [any Shape] = [Circle(r: 1), Rect(w: 2, h: 3)]
print("areas", shapes.map { String(format: "%.3f", $0.area) })
print("dynamic cast", shapes.compactMap { $0 as? Rect }.count, type(of: shapes[0]))
print("tree", [5, 3, 9, 1, 4].reduce(Tree<Int>.leaf) { $0.inserting($1) }.sorted)
do { let c = Counter(); for _ in 0..<10 { c.n += 1 } }
do { _ = try check(7) } catch { print("caught", error) }

let s = "naïve café 👩‍👩‍👧 日本語"
print("string", s.count, s.unicodeScalars.count, s.utf8.count, s.uppercased(), s.split(separator: " ").count)
print("mirror", Mirror(reflecting: Rect(w: 1, h: 2)).children.map { "\($0.label ?? "?")=\($0.value)" })
var set = Set((0..<10_000).map { $0 % 977 }); set.formIntersection(0..<100)
print("set", set.count, "dict", Dictionary(grouping: 0..<100, by: { $0 % 7 })[3]!.count)
print("sort", (0..<2000).map { ($0 * 7919) % 2003 }.sorted().prefix(5), "lazy", (1...).lazy.filter { $0 % 13 == 0 }.prefix(3).reduce(0, +))

let enc = JSONEncoder(); enc.outputFormatting = [.sortedKeys]; enc.dateEncodingStrategy = .iso8601
let doc = Doc(name: "d", tags: ["a", "b"], shapes: [Circle(r: 2)], when: Date(timeIntervalSince1970: 1_700_000_000), extra: ["x": 1])
let data = try enc.encode(doc)
print("json", String(decoding: data, as: UTF8.self))
let dec = JSONDecoder(); dec.dateDecodingStrategy = .iso8601
let back = try dec.decode(Doc.self, from: data)
print("decoded", back.shapes[0].r, back.when.timeIntervalSince1970, back.extra ?? [:])
let pdata = try PropertyListEncoder().encode(doc)
print("plist", try PropertyListDecoder().decode(Doc.self, from: pdata).tags)
print("bridge", (["k": [1, 2, 3]] as NSDictionary).value(forKeyPath: "k.@sum.self") ?? "nil", NSString(string: s).length)
print("measure", Measurement(value: 3, unit: UnitLength.kilometers).converted(to: .meters).value)
let url = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("swift.json")
try data.write(to: url)
print("file", try Data(contentsOf: url).count == data.count)
