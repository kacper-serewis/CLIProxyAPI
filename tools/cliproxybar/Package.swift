// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "CliproxyBar",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "CliproxyBarCore"),
        .executableTarget(name: "CliproxyBar", dependencies: ["CliproxyBarCore"]),
        .testTarget(name: "CliproxyBarCoreTests", dependencies: ["CliproxyBarCore"]),
    ]
)
