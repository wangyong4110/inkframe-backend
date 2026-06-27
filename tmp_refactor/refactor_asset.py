#!/usr/bin/env python3
"""Targeted asset rename for asset_service.go and asset_handler.go."""
import re
import sys

FILES = [
    "/Users/oker/meili/yong.wang2_dacs_at_okg.com/114/Documents/go/src/github.com/inkframe/inkframe-backend/internal/service/asset_service.go",
    "/Users/oker/meili/yong.wang2_dacs_at_okg.com/114/Documents/go/src/github.com/inkframe/inkframe-backend/internal/handler/asset_handler.go",
    "/Users/oker/meili/yong.wang2_dacs_at_okg.com/114/Documents/go/src/github.com/inkframe/inkframe-backend/internal/repository/asset_repository.go",
]

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

VARS = ["a", "asset"]


def apply_rules(text, var_list, rename_map):
    for var in var_list:
        for old, new in rename_map.items():
            pattern = r'(?<![A-Za-z0-9_])' + re.escape(var) + r'\.' + re.escape(old) + r'(?![A-Za-z0-9_])'
            replacement = var + '.' + new
            text = re.sub(pattern, replacement, text)
    return text


for path in FILES:
    try:
        with open(path) as f:
            orig = f.read()
    except Exception as e:
        print(f"skip {path}: {e}")
        continue
    new = apply_rules(orig, VARS, ASSET_RENAMES)
    if new != orig:
        with open(path, "w") as f:
            f.write(new)
        print(f"updated: {path}")
