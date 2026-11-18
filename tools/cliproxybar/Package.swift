// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "CliproxyBar",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "CliproxyBarCore"),
        .executableTarget(name: "CliproxyBar", dependencies: ["CliproxyBarCore"]),
        // Fixtures are read from the source tree (see ForecastGoldenTests), not bundled.
        .testTarget(name: "CliproxyBarCoreTests", dependencies: ["CliproxyBarCore"], exclude: ["Fixtures"]),
    ]
)
