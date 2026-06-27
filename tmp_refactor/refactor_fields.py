#!/usr/bin/env python3
"""Bulk field rename for inkframe-backend large JSON-merge refactoring."""
import os
import re

ROOT = "/Users/oker/meili/yong.wang2_dacs_at_okg.com/114/Documents/go/src/github.com/inkframe/inkframe-backend"

NOVEL_VARS = ["novel", "nov"]
CHAPTER_VARS = ["chapter", "ch", "chap"]
VIDEO_VARS = ["video", "vid"]
SHOT_VARS = ["shot", "sh"]
CHAR_VARS = ["char", "character"]
ASSET_VARS = ["asset"]
USER_VARS = ["user", "u"]
FORESHADOW_VARS = ["foreshadow", "fs", "fore"]
FEEDBACK_VARS = ["feedback", "fb"]
REWRITE_VARS = ["rewriteTask", "rwTask"]
REVIEW_VARS = ["review", "outlineReview", "or"]
ASYNC_VARS = ["asyncTask"]
SFX_VARS = ["sfx", "sfxItem"]
BGM_VARS = ["bgm", "bgmSeg"]
CRAWL_VARS = ["crawlJob"]
VC_VARS = ["vc", "vConfig", "videoConfig", "novelVc"]

NOVEL_RENAMES = {
    "Description": "Meta.Description",
    "Genre": "Meta.Genre",
    "Channel": "Meta.Channel",
    "TargetWordCount": "Meta.TargetWordCount",
    "TargetChapters": "Meta.TargetChapters",
    "CoverImage": "Meta.CoverImage",
    "CoreTheme": "Meta.CoreTheme",
    "PlazaTags": "Meta.PlazaTags",
    "PublishedAt": "Meta.PublishedAt",
    "Visibility": "Meta.Visibility",
    "AIModel": "AIConfig.AIModel",
    "Temperature": "AIConfig.Temperature",
    "TopP": "AIConfig.TopP",
    "MaxTokens": "AIConfig.MaxTokens",
    "TimeoutSeconds": "AIConfig.TimeoutSeconds",
    "StylePrompt": "AIConfig.StylePrompt",
    "ImageStyle": "AIConfig.ImageStyle",
    "PromptLanguage": "AIConfig.PromptLanguage",
    "ChapterMode": "AIConfig.ChapterMode",
    "AutoReviewRounds": "AIConfig.AutoReviewRounds",
    "AutoReviewMinScore": "AIConfig.AutoReviewMinScore",
    "ReviewStatus": "ReviewMeta.ReviewStatus",
    "ReviewNote": "ReviewMeta.ReviewNote",
    "ReviewedAt": "ReviewMeta.ReviewedAt",
    "ReviewedBy": "ReviewMeta.ReviewedBy",
}

CHAPTER_RENAMES = {
    "Outline": "NarrativeMeta.Outline",
    "SceneOutline": "NarrativeMeta.SceneOutline",
    "TensionLevel": "NarrativeMeta.TensionLevel",
    "ActNo": "NarrativeMeta.ActNo",
    "EmotionalTone": "NarrativeMeta.EmotionalTone",
    "HookType": "NarrativeMeta.HookType",
    "ChapterHook": "NarrativeMeta.ChapterHook",
    "ReaderExpectations": "NarrativeMeta.ReaderExpectations",
    "ChapterEndState": "NarrativeMeta.ChapterEndState",
    "PublishedAt": "QualityMeta.PublishedAt",
    "ContinuityBlocked": "QualityMeta.ContinuityBlocked",
    "QualityStatus": "QualityMeta.QualityStatus",
}

VIDEO_RENAMES = {
    "Description": "PublishMeta.Description",
    "PublishedAt": "PublishMeta.PublishedAt",
    "Visibility": "PublishMeta.Visibility",
    "HotScore": "PublishMeta.HotScore",
    "Tags": "PublishMeta.Tags",
    "Thumbnail": "PublishMeta.Thumbnail",
    "CoverURL": "PublishMeta.CoverURL",
    "FinalVideoURL": "PublishMeta.FinalVideoURL",
    "TotalShots": "PublishMeta.TotalShots",
    "Duration": "PublishMeta.Duration",
    "ReviewStatus": "PublishMeta.ReviewStatus",
    "ProviderName": "TaskMeta.ProviderName",
    "TaskID": "TaskMeta.TaskID",
    "ErrorMessage": "TaskMeta.ErrorMessage",
    "RetryCount": "TaskMeta.RetryCount",
    "Progress": "TaskMeta.Progress",
    "VideoPath": "TaskMeta.VideoPath",
    "ScriptStatus": "TaskMeta.ScriptStatus",
}

SHOT_RENAMES = {
    "Dialogue": "GenMeta.Dialogue",
    "Subtitle": "GenMeta.Subtitle",
    "Progress": "TaskMeta.Progress",
    "ErrorMessage": "TaskMeta.ErrorMessage",
    "ClipPath": "TaskMeta.ClipPath",
    "AudioPath": "TaskMeta.AudioPath",
    "ShotTaskID": "TaskMeta.ShotTaskID",
    "ShotProviderName": "TaskMeta.ShotProviderName",
    "RetryCount": "TaskMeta.RetryCount",
    "ActualVideoDuration": "TaskMeta.ActualVideoDuration",
    "TimelineStart": "TaskMeta.TimelineStart",
    "VoiceDelay": "TaskMeta.VoiceDelay",
}

CHARACTER_RENAMES = {
    "Gender": "Meta.Gender",
    "Age": "Meta.Age",
    "InnerConflict": "Meta.InnerConflict",
    "CoreDesire": "Meta.CoreDesire",
    "AppearancePrompt": "Meta.AppearancePrompt",
    "VoiceID": "VoiceConfig.VoiceID",
    "VoiceSpeed": "VoiceConfig.VoiceSpeed",
    "VoiceStyle": "VoiceConfig.VoiceStyle",
    "VoiceLanguage": "VoiceConfig.VoiceLanguage",
    "VoiceSample": "VoiceConfig.VoiceSample",
    "VoiceProfile": "VoiceConfig.VoiceProfile",
}

ASSET_RENAMES = {
    "Description": "MediaMeta.Description",
    "StorageURL": "MediaMeta.StorageURL",
    "ThumbnailURL": "MediaMeta.ThumbnailURL",
    "PreviewURL": "MediaMeta.PreviewURL",
    "SourceURL": "MediaMeta.SourceURL",
    "Attribution": "MediaMeta.Attribution",
    "Width": "MediaMeta.Width",
    "Height": "MediaMeta.Height",
    "Duration": "MediaMeta.Duration",
    "FileSize": "MediaMeta.FileSize",
    "MimeType": "MediaMeta.MimeType",
    "AspectRatio": "MediaMeta.AspectRatio",
    "DominantColor": "MediaMeta.DominantColor",
    "ColorPalette": "MediaMeta.ColorPalette",
    "Metadata": "MediaMeta.Metadata",
    "QualityScore": "QualityMeta.QualityScore",
    "QualityIssues": "QualityMeta.QualityIssues",
    "SafetyScore": "QualityMeta.SafetyScore",
    "SafetyChecked": "QualityMeta.SafetyChecked",
    "UseCount": "QualityMeta.UseCount",
    "LikeCount": "QualityMeta.LikeCount",
    "DeletedBy": "QualityMeta.DeletedBy",
    "NovelID": "QualityMeta.NovelID",
    "VideoID": "QualityMeta.VideoID",
    "ShotID": "QualityMeta.ShotID",
}

CRAWL_RENAMES = {
    "TotalFound": "Stats.TotalFound",
    "Imported": "Stats.Imported",
    "Skipped": "Stats.Skipped",
    "Failed": "Stats.Failed",
    "ErrorMsg": "Stats.ErrorMsg",
    "StartedAt": "Stats.StartedAt",
    "CompletedAt": "Stats.CompletedAt",
    "CrawlDepth": "Stats.CrawlDepth",
    "URLPattern": "Stats.URLPattern",
}

USER_RENAMES = {
    "FailedLoginCount": "SecurityMeta.FailedLoginCount",
    "LockUntil": "SecurityMeta.LockUntil",
    "LastLoginAt": "SecurityMeta.LastLoginAt",
    "EmailVerifiedAt": "SecurityMeta.EmailVerifiedAt",
    "OAuthProvider": "OAuthMeta.OAuthProvider",
    "OAuthID": "OAuthMeta.OAuthID",
}

FORESHADOW_RENAMES = {
    "Description": "Meta.Description",
    "Tags": "Meta.Tags",
    "ForeshadowType": "Meta.ForeshadowType",
    "Importance": "Meta.Importance",
    "Confidence": "Meta.Confidence",
    "LinkedHookID": "Meta.LinkedHookID",
    "LinkedArcID": "Meta.LinkedArcID",
    "CharacterIDs": "Meta.CharacterIDs",
    "ReinforcementChapters": "Meta.ReinforcementChapters",
    "PayoffQuality": "Meta.PayoffQuality",
    "PayoffNotes": "Meta.PayoffNotes",
    "ActualPayoffChapterID": "Meta.ActualPayoffChapterID",
}

REWRITE_RENAMES = {
    "SimilarityScore": "Scores.SimilarityScore",
    "LexicalSim": "Scores.LexicalSim",
    "SemanticSim": "Scores.SemanticSim",
    "StructuralSim": "Scores.StructuralSim",
    "QualityScore": "Scores.QualityScore",
    "DeaiApplied": "Scores.DeaiApplied",
    "ConsistencyIssues": "Scores.ConsistencyIssues",
    "ErrorMsg": "Scores.ErrorMsg",
    "AttemptContent": "Scores.AttemptContent",
}

FEEDBACK_RENAMES = {
    "AdminNote": "AdminMeta.AdminNote",
    "ReplyContent": "AdminMeta.ReplyContent",
    "RepliedAt": "AdminMeta.RepliedAt",
    "ResolvedAt": "AdminMeta.ResolvedAt",
    "PageURL": "AdminMeta.PageURL",
    "UserAgent": "AdminMeta.UserAgent",
    "Screenshots": "AdminMeta.Screenshots",
    "ContactEmail": "AdminMeta.ContactEmail",
}

OUTLINE_REVIEW_RENAMES = {
    "StructureScore": "Scores.StructureScore",
    "PacingScore": "Scores.PacingScore",
    "ContinuityScore": "Scores.ContinuityScore",
    "CharacterScore": "Scores.CharacterScore",
    "ConflictScore": "Scores.ConflictScore",
    "HookScore": "Scores.HookScore",
    "IssuesJSON": "Content.IssuesJSON",
    "HighlightsJSON": "Content.HighlightsJSON",
    "Suggestion": "Content.Suggestion",
}

ASYNC_RENAMES = {
    "MaxRetries": "DLQ.MaxRetries",
    "FailureLog": "DLQ.FailureLog",
}

SFX_RENAMES = {
    "FadeInMs": "Playback.FadeInMs",
    "FadeOutMs": "Playback.FadeOutMs",
    "PlayCount": "Playback.PlayCount",
}

BGM_RENAMES = {
    "SearchQueries": "TrackMeta.SearchQueries",
    "TrackName": "TrackMeta.TrackName",
    "TrackArtist": "TrackMeta.TrackArtist",
    "Source": "TrackMeta.Source",
    "Mood": "TrackMeta.Mood",
    "Tempo": "TrackMeta.Tempo",
    "DuckingEnabled": "Ducking.Enabled",
    "DuckingLevel": "Ducking.Level",
}

VC_RENAMES = {
    "VideoType": "Config.VideoType",
    "VideoResolution": "Config.VideoResolution",
    "VideoFPS": "Config.VideoFPS",
    "VideoAspectRatio": "Config.VideoAspectRatio",
    "CharConsistencyWeight": "Config.CharConsistencyWeight",
    "NarrationVoice": "Config.NarrationVoice",
    "SubtitleEnabled": "Config.SubtitleEnabled",
    "SubtitlePosition": "Config.SubtitlePosition",
    "SubtitleFontSize": "Config.SubtitleFontSize",
    "SubtitleColor": "Config.SubtitleColor",
    "SubtitleBgStyle": "Config.SubtitleBgStyle",
    "ColorGrade": "Config.ColorGrade",
    "ContrastLevel": "Config.ContrastLevel",
    "Saturation": "Config.Saturation",
    "FilmGrain": "Config.FilmGrain",
    "Vignette": "Config.Vignette",
    "ChromaticAberration": "Config.ChromaticAberration",
    "KlingProForAction": "Config.KlingProForAction",
    "KlingModel": "Config.KlingModel",
    "ThreeDEnabled": "Config.ThreeDEnabled",
    "SubtitleStyle": "Config.SubtitleStyle",
    "SubtitleFont": "Config.SubtitleFont",
}


def apply_rules(text, var_list, rename_map):
    for var in var_list:
        for old, new in rename_map.items():
            pattern = r'(?<![A-Za-z0-9_])' + re.escape(var) + r'\.' + re.escape(old) + r'(?![A-Za-z0-9_])'
            replacement = var + '.' + new
            text = re.sub(pattern, replacement, text)
    return text


def process_file(path):
    if not path.endswith(".go"):
        return False
    if "/internal/model/" in path:
        return False
    if "/tmp_refactor/" in path:
        return False
    try:
        with open(path, "r") as f:
            original = f.read()
    except Exception:
        return False
    text = original

    text = apply_rules(text, NOVEL_VARS, NOVEL_RENAMES)
    text = apply_rules(text, CHAPTER_VARS, CHAPTER_RENAMES)
    text = apply_rules(text, VIDEO_VARS, VIDEO_RENAMES)
    text = apply_rules(text, SHOT_VARS, SHOT_RENAMES)
    text = apply_rules(text, CHAR_VARS, CHARACTER_RENAMES)
    text = apply_rules(text, ASSET_VARS, ASSET_RENAMES)
    text = apply_rules(text, CRAWL_VARS, CRAWL_RENAMES)
    text = apply_rules(text, USER_VARS, USER_RENAMES)
    text = apply_rules(text, FORESHADOW_VARS, FORESHADOW_RENAMES)
    text = apply_rules(text, REWRITE_VARS, REWRITE_RENAMES)
    text = apply_rules(text, FEEDBACK_VARS, FEEDBACK_RENAMES)
    text = apply_rules(text, REVIEW_VARS, OUTLINE_REVIEW_RENAMES)
    text = apply_rules(text, ASYNC_VARS, ASYNC_RENAMES)
    text = apply_rules(text, SFX_VARS, SFX_RENAMES)
    text = apply_rules(text, BGM_VARS, BGM_RENAMES)
    text = apply_rules(text, VC_VARS, VC_RENAMES)

    if text != original:
        with open(path, "w") as f:
            f.write(text)
        return True
    return False


def main():
    changed = 0
    for dirpath, dirs, files in os.walk(ROOT):
        if "/.git" in dirpath:
            continue
        if "/internal/model" in dirpath:
            continue
        if "/bin/" in dirpath:
            continue
        if "/tmp_refactor" in dirpath:
            continue
        for fn in files:
            if not fn.endswith(".go"):
                continue
            path = os.path.join(dirpath, fn)
            if process_file(path):
                changed += 1
                print("changed:", path)
    print(f"Total changed: {changed}")


if __name__ == "__main__":
    main()
