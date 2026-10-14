import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#elseif canImport(Musl)
import Musl
#endif

/// Whether a loopback TCP port is free, for choosing the daemon's port on first run.
public enum PortProbe {
    /// True when nothing on 127.0.0.1 holds `port`: a bind succeeds. There is a window
    /// between this and conductord binding, and if something takes it then, conductord says
    /// so in its log.
    public static func isFree(_ port: Int) -> Bool {
        guard port > 0 && port < 65536 else { return false }
        #if canImport(Darwin)
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        #else
        let fd = socket(AF_INET, Int32(SOCK_STREAM.rawValue), 0)
        #endif
        guard fd >= 0 else { return false }
        defer { close(fd) }
        var addr = sockaddr_in()
        #if canImport(Darwin)
        addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        #endif
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = in_port_t(UInt16(port).bigEndian)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) == 0
            }
        }
        return bound
    }

    /// The first free port of `count` starting at `start`, or nil.
    public static func firstFree(from start: Int, count: Int = 20, isFree: (Int) -> Bool = PortProbe.isFree) -> Int? {
        (start..<(start + count)).first(where: isFree)
    }
}
