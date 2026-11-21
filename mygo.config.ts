// The configuration of the app, which `mygo dev` and `mygo build` evaluate with
// Bun, of the command running. The development app (`mygo dev`) opens links of a
// scheme of its own, for the installed app to keep mygo-music:// links.
export default ({ command }: { command: string }) => ({
  name: "MyGo Music",
  identifier: "dev.egoist.music",
  slug: "mygo-music",
  version: "0.1.32",
  out: "build",
  resourcesExclude: [
    "windows-amd64/demucs/.runtime",
    "windows-amd64/demucs/__pycache__",
    ...(command === "build" && process.env.MYGO_MUSIC_BUNDLE_CUDA !== "1"
      ? ["windows-amd64/demucs/native-v1/cuda"]
      : []),
  ],
  urlSchemes: [command === "dev" ? "mygo-music-dev" : "mygo-music"],
  updates: {
    publicKey: "WzJsOGNIuf6mcEqo5ff8jub+NoQQOEk4JXreLPYjgyQ=",
    github: process.env.MYGO_MUSIC_RELEASE_REPOSITORY || "egoist/mygo-music",
  },
  macos: {
    icon: "icon/AppIcon.icon",
    minimumSystemVersion: "13.0",
    signingIdentity: "Developer ID Application: xinyu zeng (GJE9R5VE87)",
    notarize: { keychainProfile: "notary" },
  },
  linux: {
    comment: "A music player for your files and more",
    categories: ["AudioVideo", "Audio", "Player"],
    webview: false,
  },
});
