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
  url "https://github.com/gauravthakur02/kubesage/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "72640cc70b4492ae7409498454183c68f1cce2f4a9186916e90870d85788721d"
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
