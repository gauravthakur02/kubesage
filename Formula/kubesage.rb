# Homebrew formula: builds kubesage from source on your Mac, so macOS never shows the
# "Apple could not verify" warning (that only happens to binaries downloaded in a browser).
#
#   brew tap gauravthakur02/kubesage https://github.com/gauravthakur02/kubesage
#   brew trust gauravthakur02/kubesage   # Homebrew 7+: trust the tap once
#   brew install kubesage
#
# The release workflow updates url and sha256 for every new tag.
class Kubesage < Formula
  desc "Kubernetes IDE for your terminal with an AI copilot"
  homepage "https://github.com/gauravthakur02/kubesage"
  url "https://github.com/gauravthakur02/kubesage/archive/refs/tags/v0.5.0.tar.gz"
  sha256 "31d74f073dbcfe9c7cdc22b39175a3e83bc65279ce6fb29d81301345484663c1"
  license "MIT"
  head "https://github.com/gauravthakur02/kubesage.git", branch: "main"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}")
    if version >= Version.new("0.3.0") || build.head?
      generate_completions_from_executable(bin/"kubesage", "completion", shells: [:bash, :zsh, :fish])
      (man1/"kubesage.1").write Utils.safe_popen_read(bin/"kubesage", "man")
    end
  end

  def caveats
    <<~TEXT
      Start with: kubesage   (add --readonly to disable every change)
      The first start shows a one-minute tour; :tour shows it again.
    TEXT
  end

  test do
    ENV["KUBESAGE_NO_UPDATE_CHECK"] = "1"
    ENV["NO_COLOR"] = "1"
    assert_match version.to_s, shell_output("#{bin}/kubesage version")
  end
end
