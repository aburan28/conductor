// swift-tools-version:5.9
//
// Conductor.app: the control plane, its database, and the dashboard as one Mac app.
// docs/MACOS_APP.md is the plan; macos/README.md says how to build, test, package and sign.
//
// A plain SwiftPM package rather than an Xcode project, so it builds with the Command Line
// Tools alone:
//
//   ConductorKit           every piece of logic: launchd plists, the supervisor, the API and
//                          event-stream clients, the storage settings, the decoders for the
//                          CLI's --json output. It builds and is tested on Linux too, so CI
//                          can check it without a Mac.
//   ConductorKeychainACL   one function that builds a Keychain access list naming trusted
//                          applications. Its APIs are deprecated (and still the only way to
//                          do it), so the target is compiled with warnings suppressed and
//                          nothing else lives in it.
import PackageDescription

var products: [Product] = [
    .library(name: "ConductorKit", targets: ["ConductorKit"]),
]
var dependencies: [Package.Dependency] = []
var targets: [Target] = [
    .target(
        name: "ConductorKeychainACL",
        path: "Sources/ConductorKeychainACL",
        swiftSettings: [.unsafeFlags(["-suppress-warnings"])],
        linkerSettings: [.linkedFramework("Security", .when(platforms: [.macOS]))]
    ),
    .target(
        name: "ConductorKit",
        dependencies: ["ConductorKeychainACL"],
        path: "Sources/ConductorKit",
        linkerSettings: [.linkedFramework("Security", .when(platforms: [.macOS]))]
    ),
    .testTarget(
        name: "ConductorKitTests",
        dependencies: ["ConductorKit"],
        path: "Tests/ConductorKitTests"
    ),
]

let package = Package(
    name: "Conductor",
    platforms: [.macOS(.v13)],
    products: products,
    dependencies: dependencies,
    targets: targets
)
