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
//   Conductor              the SwiftUI app: menu bar, dashboard window, sheets, settings. It
//                          is macOS-only and is declared only when the manifest is evaluated
//                          on a Mac, so `swift build` and `swift test` on Linux never see it.
//
// Sparkle is the one dependency, and only the app links it.
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

#if os(macOS)
// Exact, not `from:`: Sparkle ships inside the signed app and runs its installer with the
// user's rights, so a new release of it is a change to review, not something a resolve picks
// up unread.
dependencies.append(.package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.10.0"))
products.append(.executable(name: "Conductor", targets: ["Conductor"]))
targets.append(
    .executableTarget(
        name: "Conductor",
        dependencies: [
            "ConductorKit",
            .product(name: "Sparkle", package: "Sparkle"),
        ],
        path: "Sources/Conductor",
        swiftSettings: [.unsafeFlags(["-parse-as-library"])],
        linkerSettings: [
            // build.sh copies Sparkle.framework into Contents/Frameworks.
            .unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"]),
        ]
    )
)
#endif

let package = Package(
    name: "Conductor",
    platforms: [.macOS(.v13)],
    products: products,
    dependencies: dependencies,
    targets: targets
)
