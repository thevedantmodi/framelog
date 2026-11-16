# Template for Casks/framelog.rb in the thevedantmodi/homebrew-framelog tap.
# The release workflow rewrites only the version and sha256 lines in the tap,
# so any other change here must also be made in the tap by hand.
cask "framelog" do
  version "0.16.0"
  sha256 "ecb7407b2014df84b9c61d5b98d3f4bb4e7777041640717da3e83b7e7b95f6f9"

  url "https://github.com/thevedantmodi/framelog/releases/download/v#{version}/Framelog-#{version}.dmg"
  name "Framelog"
  desc "Automated photo pipeline with SD card import, Lightroom XMP versioning, and git backup"
  homepage "https://github.com/thevedantmodi/framelog"

  depends_on formula: "exiftool"
  depends_on formula: "git"
  depends_on formula: "rclone"

  app "Framelog.app"

  caveats <<~EOS
    Open Framelog from Applications, then click "Install Core…" in its menu
    bar menu to start the background daemon.
  EOS

  zap trash: [
    "~/Library/Application Support/Framelog",
    "~/Library/Logs/Framelog",
    "~/Library/LaunchAgents/com.framelog.core.plist",
  ]
end
