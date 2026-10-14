import Foundation

/// Any JSON value, decoded without a schema.
///
/// The app reads the output of commands that are being written at the same time as it is
/// (`conductor db status --json`, `conductor db backups --json`). Decoding those into fixed
/// structs would turn every field the Go side adds, renames or leaves out into a failure, so
/// they are read into this and looked at leniently. It also lets the app rewrite
/// `storage.json` while keeping fields it does not know about.
public enum JSONValue: Equatable, Codable, Sendable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self = .null
        } else if let b = try? c.decode(Bool.self) {
            self = .bool(b)
        } else if let n = try? c.decode(Double.self) {
            self = .number(n)
        } else if let s = try? c.decode(String.self) {
            self = .string(s)
        } else if let a = try? c.decode([JSONValue].self) {
            self = .array(a)
        } else if let o = try? c.decode([String: JSONValue].self) {
            self = .object(o)
        } else {
            throw DecodingError.dataCorruptedError(in: c, debugDescription: "not a JSON value")
        }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let b): try c.encode(b)
        case .number(let n):
            // Whole numbers go out as integers, so a round trip does not turn 24 into 24.0.
            if n.rounded() == n, abs(n) < 9.0e15 {
                try c.encode(Int64(n))
            } else {
                try c.encode(n)
            }
        case .string(let s): try c.encode(s)
        case .array(let a): try c.encode(a)
        case .object(let o): try c.encode(o)
        }
    }

    public static func decode(_ data: Data) throws -> JSONValue {
        try JSONDecoder().decode(JSONValue.self, from: data)
    }

    public subscript(key: String) -> JSONValue? {
        if case .object(let o) = self { return o[key] }
        return nil
    }

    public var objectValue: [String: JSONValue]? {
        if case .object(let o) = self { return o }
        return nil
    }

    public var arrayValue: [JSONValue]? {
        if case .array(let a) = self { return a }
        return nil
    }

    public var stringValue: String? {
        if case .string(let s) = self { return s }
        return nil
    }

    public var boolValue: Bool? {
        if case .bool(let b) = self { return b }
        return nil
    }

    public var doubleValue: Double? {
        if case .number(let n) = self { return n }
        return nil
    }

    public var intValue: Int? {
        if case .number(let n) = self, n.rounded() == n, abs(n) < 9.0e15 { return Int(n) }
        return nil
    }

    public var isNull: Bool { self == .null }

    /// A short human rendering of a scalar: what a status row shows. Arrays and objects say
    /// how many items they hold rather than dumping themselves into a label.
    public var displayText: String {
        switch self {
        case .null: return "—"
        case .bool(let b): return b ? "yes" : "no"
        case .number(let n):
            if let i = intValue { return String(i) }
            return String(format: "%.2f", n)
        case .string(let s): return s.isEmpty ? "—" : s
        case .array(let a): return a.count == 1 ? "1 item" : "\(a.count) items"
        case .object(let o): return o.count == 1 ? "1 field" : "\(o.count) fields"
        }
    }

    /// The first of `keys` present with a non-null value: how the lenient decoders try the
    /// spellings a field might have.
    public func first(_ keys: String...) -> JSONValue? {
        first(keys)
    }

    public func first(_ keys: [String]) -> JSONValue? {
        guard case .object(let o) = self else { return nil }
        for k in keys {
            if let v = o[k], v != .null { return v }
        }
        return nil
    }
}
