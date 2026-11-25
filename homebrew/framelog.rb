# Template for Casks/framelog.rb in the thevedantmodi/homebrew-framelog tap.
# The release workflow rewrites only the version and sha256 lines in the tap,
# so any other change here must also be made in the tap by hand.
cask "framelog" do
  version "0.17.0"
  sha256 "e4e464325316862ae82693aa8aeb12e7cb1662382f295d30ee4e782a36ce2359"

  url "https://github.com/thevedantmodi/framelog/releases/download/v#{version}/Framelog-#{version}.dmg"
  name "Framelog"
  desc "Automated photo pipeline with SD card import, verified backups and safe-to-format checks"
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
